// SPDX-License-Identifier: AGPL-3.0-only

// Out of office (#50), the pure half: whole local days to instants and back,
// the form's own checks, and the lines the row prints (drift, the contacts
// bound, the state line).

import { describe, expect, it } from "vitest";

import type { OutOfOffice } from "../../../core/jmap/types";
import { contactsLine, formFrom, isDrift, outOfOfficeFacts, startOfLocalDay, statusLine, updateFrom } from "./outOfOffice";
import type { OutOfOfficeForm } from "./outOfOffice";

function obj(over: Partial<OutOfOffice> = {}): OutOfOffice {
  return {
    id: "A1", accountName: "Main", isEnabled: false, fromDate: null, toDate: null, subject: "Away",
    textBody: "I am away.", replyDays: 7, fromAddress: "me@example.com", contactsOnly: false,
    addresses: ["me@example.com"], isSupported: true, serverState: "off", serverDetail: "",
    checkedAt: null, contactsIncluded: null, contactsTotal: null, maxContacts: 2000, ...over,
  };
}

function form(over: Partial<OutOfOfficeForm> = {}): OutOfOfficeForm {
  return { ...formFrom(obj()), isEnabled: true, ...over };
}

const NOW = new Date(2026, 10, 1, 12, 0, 0); // 1 Nov 2026, local noon

describe("the window is whole local days", () => {
  it("starts at local midnight of the first day and ends at the midnight AFTER the last", () => {
    const u = updateFrom(form({ firstDay: "2026-11-20", lastDay: "2026-11-22" }), NOW);
    if ("error" in u) throw new Error(u.error);
    expect(new Date(u.fromDate as string).getTime()).toBe(new Date(2026, 10, 20).getTime());
    expect(new Date(u.toDate as string).getTime()).toBe(new Date(2026, 10, 23).getTime());
    expect(u.fromDate).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  });

  it("round-trips: the form a saved object opens with is the form that saved it", () => {
    const u = updateFrom(form({ firstDay: "2026-11-20", lastDay: "2026-11-22" }), NOW);
    if ("error" in u) throw new Error(u.error);
    const back = formFrom(obj({ fromDate: u.fromDate, toDate: u.toDate }));
    expect([back.firstDay, back.lastDay]).toEqual(["2026-11-20", "2026-11-22"]);
  });

  it("an empty day is open-ended", () => {
    const u = updateFrom(form({ firstDay: "", lastDay: "" }), NOW);
    if ("error" in u) throw new Error(u.error);
    expect([u.fromDate, u.toDate]).toEqual([null, null]);
  });

  it("a one-day window covers that whole day", () => {
    const u = updateFrom(form({ firstDay: "2026-11-20", lastDay: "2026-11-20" }), NOW);
    if ("error" in u) throw new Error(u.error);
    expect(new Date(u.toDate as string).getTime() - new Date(u.fromDate as string).getTime()).toBe(24 * 3600 * 1000);
  });

  it("rejects a non-date", () => {
    expect(startOfLocalDay("20/11/2026")).toBeNull();
    expect(updateFrom(form({ lastDay: "soon" }), NOW)).toHaveProperty("error");
  });
});

describe("the form's own checks", () => {
  it.each([
    ["last before first", { firstDay: "2026-11-22", lastDay: "2026-11-20" }],
    ["last day passed, turning on", { lastDay: "2026-10-30" }],
    ["empty message, turning on", { textBody: "  " }],
    ["0 days", { replyDays: 0 }],
    ["31 days", { replyDays: 31 }],
    ["fractional days", { replyDays: 1.5 }],
  ])("refuses %s", (_name, over) => {
    expect(updateFrom(form(over as Partial<OutOfOfficeForm>), NOW)).toHaveProperty("error");
  });

  it("saving OFF with a past window or an empty message is allowed (it only stores the text)", () => {
    expect(updateFrom(form({ isEnabled: false, lastDay: "2026-10-30", textBody: "" }), NOW)).not.toHaveProperty("error");
  });
});

describe("what the row prints", () => {
  it("drift is a warning carrying the server's sentence", () => {
    for (const s of ["displaced", "missing", "modified"] as const) {
      const o = obj({ isEnabled: true, serverState: s, serverDetail: "The filter script \"x\" is active." });
      expect(isDrift(o)).toBe(true);
      expect(statusLine(o)).toEqual({ text: "The filter script \"x\" is active.", warn: true });
    }
    expect(isDrift(obj({ isEnabled: true, serverState: "active" }))).toBe(false);
    expect(statusLine(obj({ serverState: "error", serverDetail: "" })).warn).toBe(true);
  });

  it("the contacts bound is stated, never hidden", () => {
    expect(contactsLine(obj({ contactsOnly: true, contactsIncluded: 1412, contactsTotal: 2650 }))).toBe(
      "The reply covers 1,412 of 2,650; the rest get no reply.",
    );
    expect(contactsLine(obj({ contactsOnly: true, contactsIncluded: 3, contactsTotal: 3 }))).toBe(
      "The reply covers all 3 of them.",
    );
    expect(contactsLine(obj({ contactsOnly: false, contactsIncluded: 3, contactsTotal: 3 }))).toBeNull();
  });

  it("the row's line: nothing until read, then which accounts reply", () => {
    expect(outOfOfficeFacts(null)).toEqual([]);
    expect(outOfOfficeFacts([obj()])).toEqual(["off"]);
    expect(outOfOfficeFacts([obj({ isEnabled: true }), obj({ id: "B", accountName: "Work" })])).toEqual(["on for Main"]);
    expect(outOfOfficeFacts([obj({ isEnabled: true, serverState: "displaced" })])).toEqual([
      "on for Main",
      "changed on the server",
    ]);
  });

  it("an unsupported account says why", () => {
    expect(statusLine(obj({ isSupported: false, unsupportedReason: "Gmail offers no ManageSieve service" })).text).toBe(
      "Not available for this account: Gmail offers no ManageSieve service.",
    );
  });
});
