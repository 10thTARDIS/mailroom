// SPDX-License-Identifier: AGPL-3.0-only

// The invitation card's words (#725): time in the reader's zone, a floating
// time never converted, recurrence summarised, and the one state line.

import { describe, expect, it } from "vitest";

import type { CalendarInvite, CalendarTime } from "../../../core/jmap/types";
import { formatWhen, inviteState, personLabel, repeatsText, replySummary, responseText } from "./invite";

const utc = (s: string): CalendarTime => ({ kind: "utc", utc: s, date: null, local: null, timeZone: null });
const date = (s: string): CalendarTime => ({ kind: "date", utc: null, date: s, local: null, timeZone: null });
const floating = (s: string, zone: string | null): CalendarTime => ({
  kind: "floating",
  utc: null,
  date: null,
  local: s,
  timeZone: zone,
});

describe("formatWhen", () => {
  it("shows an instant in the READER's zone, and names the zone", () => {
    const ny = formatWhen(utc("2026-10-20T15:00:00Z"), utc("2026-10-20T15:30:00Z"), {
      locale: "en-GB",
      timeZone: "America/New_York",
    });
    expect(ny).toContain("11:00");
    expect(ny).toContain("11:30");
    expect(ny).not.toContain("15:00");
    expect(ny).toMatch(/GMT-4|EDT/);
    // The SAME instant for a reader in UTC — the control that proves the zone,
    // not the string, decided the hour above.
    const z = formatWhen(utc("2026-10-20T15:00:00Z"), utc("2026-10-20T15:30:00Z"), { locale: "en-GB", timeZone: "UTC" });
    expect(z).toContain("15:00");
    expect(z).toContain("15:30");
  });

  it("names both days when an event crosses midnight in the reader's zone", () => {
    const s = formatWhen(utc("2026-10-20T22:00:00Z"), utc("2026-10-21T01:00:00Z"), { locale: "en-GB", timeZone: "UTC" });
    expect(s).toContain("20 Oct");
    expect(s).toContain("21 Oct");
  });

  it("an all-day event: DTEND is exclusive, so one day reads as one day", () => {
    // Intl's exact punctuation varies by ICU version, so the assertion is on
    // the parts that carry meaning: one date, and the all-day marker.
    const one = formatWhen(date("2026-12-24"), date("2026-12-25"), { locale: "en-GB" }) ?? "";
    expect(one).toMatch(/^Thu,? 24 Dec 2026 \(all day\)$/);
    const three = formatWhen(date("2026-12-24"), date("2026-12-27"), { locale: "en-GB" });
    expect(three).toContain("24 Dec");
    expect(three).toContain("26 Dec");
    expect(three).not.toContain("27 Dec");
  });

  it("a FLOATING time is shown as written, zone named, and never converted", () => {
    const a = formatWhen(floating("2026-10-20T09:00:00", "Mars/Olympus_Mons"), floating("2026-10-20T10:00:00", null), {
      locale: "en-GB",
      timeZone: "Asia/Tokyo",
    });
    expect(a).toContain("09:00");
    expect(a).toContain("10:00");
    expect(a).toContain("Mars/Olympus_Mons");
    expect(a).toContain("shown as written");
    const b = formatWhen(floating("2026-10-20T09:00:00", null), null, { locale: "en-GB", timeZone: "America/Los_Angeles" });
    expect(b).toContain("09:00");
    expect(b).toContain("no time zone given");
  });

  it("a zone name carrying a direction override is stripped of it", () => {
    const s = formatWhen(floating("2026-10-20T09:00:00", "Evil‮Zone"), null, { locale: "en-GB" });
    expect(s).not.toContain("‮");
    expect(s).toContain("EvilZone");
  });

  it("no start, no line", () => {
    expect(formatWhen(null, null)).toBeNull();
  });
});

describe("repeatsText", () => {
  const rec = (over: Partial<NonNullable<CalendarInvite["recurrence"]>>) => ({
    freq: "WEEKLY",
    interval: 1,
    count: null,
    until: null,
    byDay: [],
    text: "server text",
    ...over,
  });
  it("summarises, in the reader's language, from the structured fields", () => {
    expect(repeatsText(rec({ byDay: ["MO", "WE"] }), { locale: "en" })).toBe("Repeats weekly on Monday, Wednesday");
    expect(repeatsText(rec({ interval: 2, count: 10 }), { locale: "en" })).toBe("Repeats every 2 weeks, 10 times");
    expect(repeatsText(rec({ freq: "MONTHLY", byDay: ["-1FR"] }), { locale: "en" })).toBe("Repeats monthly on last Friday");
    expect(repeatsText(rec({ until: utc("2026-12-16T17:00:00Z") }), { locale: "en-GB", timeZone: "UTC" })).toMatch(
      /^Repeats weekly, until Wed,? 16 Dec 2026$/,
    );
  });
  it("a frequency it cannot name still says it repeats", () => {
    expect(repeatsText(rec({ freq: "SECONDLY" }))).toBe("Repeats");
    expect(repeatsText(rec({ freq: null }))).toBe("Repeats");
  });
  it("no rule, no line", () => {
    expect(repeatsText(null)).toBeNull();
  });
});

describe("inviteState", () => {
  const base: CalendarInvite = { problem: null, method: "REQUEST", sequence: 0 };
  it("cancelled beats newer beats updates beats a bare SEQUENCE", () => {
    expect(inviteState({ ...base, cancelled: true, newerEmailId: "N", updatesEmailId: "U" })).toBe("cancelled");
    expect(inviteState({ ...base, newerEmailId: "N", updatesEmailId: "U" })).toBe("newer");
    expect(inviteState({ ...base, updatesEmailId: "U", sequence: 3 })).toBe("updates");
    expect(inviteState({ ...base, sequence: 3 })).toBe("updated");
    expect(inviteState(base)).toBeNull();
    // A REPLY's SEQUENCE is the event's, not a sign of an update.
    expect(inviteState({ ...base, method: "REPLY", sequence: 3 })).toBeNull();
  });
});

describe("responseText", () => {
  const r = (partstat: "ACCEPTED" | "TENTATIVE" | "DECLINED", sequence: number) => ({
    partstat,
    sequence,
    respondedAt: "2026-10-17T12:00:00Z",
    submissionId: "S",
    emailId: "E",
  });
  it("says what the user answered, and when it was an answer to an earlier version", () => {
    expect(responseText({ problem: null, sequence: 0, response: r("ACCEPTED", 0) })).toBe("You accepted.");
    expect(responseText({ problem: null, sequence: 0, response: r("TENTATIVE", 0) })).toBe("You said maybe.");
    expect(responseText({ problem: null, sequence: 2, response: r("DECLINED", 1) })).toBe(
      "You declined. That was for an earlier version.",
    );
    expect(responseText({ problem: null, sequence: 0, response: null })).toBeNull();
  });
});

describe("people", () => {
  it("name and address, bidi-stripped; the address alone when there is no name", () => {
    expect(personLabel({ name: "Ann", email: "ann@example.com" })).toBe("Ann <ann@example.com>");
    expect(personLabel({ name: null, email: "ann@example.com" })).toBe("ann@example.com");
    expect(personLabel({ name: "A‮nn", email: "ann@example.com" })).toBe("Ann <ann@example.com>");
  });
  it("a reply says who answered and how", () => {
    expect(
      replySummary([
        { name: "Chen Wei", email: "chen@example.org", partstat: "DECLINED", role: "REQ-PARTICIPANT" },
        { name: null, email: "dee@example.org", partstat: "ACCEPTED", role: "REQ-PARTICIPANT" },
      ]),
    ).toEqual(["Chen Wei <chen@example.org> declined.", "dee@example.org accepted."]);
  });
});
