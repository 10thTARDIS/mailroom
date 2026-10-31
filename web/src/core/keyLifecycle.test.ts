// SPDX-License-Identifier: AGPL-3.0-only

// The v1.93 final scope's client logic (#815, #813, #812, #817, #818) against
// responses captured from the C1 server (keyHistoryFixtures.testdata.ts): the
// date the user picks and what goes on the wire, the server's own range rule,
// user IDs as a title, the retirement date read honestly, a correspondent
// key's history in words, a refresh's answer, and what a failed replace left.
// The timezone half of the date mapping is keyLifecycle.tz.test.ts.

import { describe, expect, it } from "vitest";

import { JmapMethodError } from "./jmap/client";
import {
  C1_CONTACTS,
  C1_CONTACT_KEY_ALICE,
  C1_CONTACT_KEY_BOB_REMOVED,
  C1_CONTACT_KEY_CAROL_EXPIRED,
  C1_EXTEND_DATE_SENT,
  C1_EXTEND_TO_A_DATE,
  C1_GENERATE_DATE_SENT,
  C1_HISTORY_BOB,
  C1_KEY_NOT_LISTED,
  C1_KEY_ON_A_DATE,
  C1_KEY_REPLACED,
  C1_KEY_REPLACEMENT,
  C1_KEY_RETIRED_NOT_RECORDED,
  C1_KEY_REVOKED,
  C1_KEY_TWO_USER_IDS,
  C1_METHOD_ERRORS,
  C1_REFRESH_ALICE_DIFFERENT,
  C1_REFRESH_ALICE_NOT_FOUND,
  C1_REFRESH_ALICE_UNCHANGED,
  C1_REFRESH_ALICE_UNREACHABLE,
  C1_REFRESH_CAROL_RENEWED,
  C1_REPLACE_FAILED_AFTER_GENERATION,
} from "./keyHistoryFixtures.testdata";
import {
  addCalendarDays,
  calendarDateLong,
  decideAddress,
  endOfUtcDay,
  exactDateBounds,
  exactDateExpiresSoon,
  exactDateProblem,
  heldKeyForAddress,
  isPastKey,
  keyHistoryLines,
  newKeyExpiryArg,
  refreshIsProminent,
  refreshOutcome,
  replaceLeftNewKey,
  retiredFact,
  titleUserIds,
  userIdText,
} from "./keyLifecycle";

const at = (iso: string) => new Date(iso);

describe("an exact expiry date (#813)", () => {
  it("is sent as the picked calendar date, verbatim, and only that", () => {
    const now = at("2026-10-21T12:00:00Z");
    expect(newKeyExpiryArg("date", "2026-11-04", now)).toEqual({ expiresAt: "2026-11-04" });
    expect(newKeyExpiryArg("date", " 2026-11-04 ", now)).toEqual({ expiresAt: "2026-11-04" });
    // The presets are unchanged: two years is the server's default (nothing sent).
    expect(newKeyExpiryArg("2y", "2026-11-04", now)).toEqual({});
    expect(newKeyExpiryArg("1y", "", now)).toEqual({ expiresDays: 365 });
    expect(newKeyExpiryArg("never", "", now)).toEqual({ expiresDays: 0 });
  });

  it("means what the server made of it: the end of that day in UTC", () => {
    // CAPTURED: the server was sent this date and put this instant on the key.
    expect(new Date(endOfUtcDay(C1_GENERATE_DATE_SENT)!).toISOString().replace(".000", "")).toBe(
      C1_KEY_ON_A_DATE.expiresAt,
    );
    expect(new Date(endOfUtcDay(C1_EXTEND_DATE_SENT)!).toISOString().replace(".000", "")).toBe(
      C1_EXTEND_TO_A_DATE.expiresAt,
    );
  });

  it("is refused as the server refuses it: less than a day, or more than 3650 days", () => {
    const now = at("2026-10-21T12:00:00Z");
    expect(exactDateProblem("2026-10-21", now)).toBe("tooSoon");
    // Tomorrow ends 35h59m from noon UTC today: allowed.
    expect(exactDateProblem("2026-10-22", now)).toBeNull();
    expect(exactDateProblem("2036-10-17", now)).toBeNull();
    expect(exactDateProblem("2036-10-18", now)).toBe("tooFar");
    expect(exactDateProblem("2026-02-30", now)).toBe("invalid");
    expect(exactDateProblem("", now)).toBe("invalid");
    expect(newKeyExpiryArg("date", "2026-10-21", now)).toBeNull();
  });

  it("offers from tomorrow to ten years out, inside the server's rule at both ends", () => {
    const now = at("2026-10-21T12:00:00Z");
    const { min, max } = exactDateBounds(now);
    expect(exactDateProblem(min, now)).toBeNull();
    expect(exactDateProblem(max, now)).toBeNull();
    expect(exactDateProblem(addCalendarDays(max, 1), now)).toBe("tooFar");
  });

  it("is said as the day itself, and inside the warning window it says so", () => {
    expect(calendarDateLong("2026-11-04")).toBe("4 Nov 2026");
    const now = at("2026-10-21T12:00:00Z");
    expect(exactDateExpiresSoon("2026-11-04", now, 30)).toBe(true);
    expect(exactDateExpiresSoon("2027-11-04", now, 30)).toBe(false);
    // No threshold from the server, no judgement.
    expect(exactDateExpiresSoon("2026-11-04", now, null)).toBe(false);
    // CAPTURED: the server agrees, 14 days out is expiringSoon at once.
    expect(C1_KEY_ON_A_DATE.expiringSoon).toBe(true);
  });
});

describe("user IDs (#812)", () => {
  it("the primary is the title, the rest are listed in order", () => {
    const { title, others } = titleUserIds(C1_KEY_TWO_USER_IDS.userIds);
    expect(userIdText(title!)).toBe("Reader at work <reader@work.example>");
    expect(others.map(userIdText)).toEqual(["Reader Example <reader@example.com>"]);
    // DERIVED: the same user IDs in the other order. The flag, not the
    // position, says which is primary.
    const reversed = titleUserIds([...C1_KEY_TWO_USER_IDS.userIds!].reverse());
    expect(userIdText(reversed.title!)).toBe("Reader at work <reader@work.example>");
    expect(reversed.others.map(userIdText)).toEqual(["Reader Example <reader@example.com>"]);
  });

  it("an address-only user ID is just the address; none at all is no title", () => {
    expect(userIdText(C1_KEY_REPLACEMENT.userIds![0]!)).toBe("reader@example.com");
    expect(titleUserIds(undefined)).toEqual({ title: null, others: [] });
    expect(titleUserIds([])).toEqual({ title: null, others: [] });
  });

  it("keeps a stranger's markup as characters, and the revoked flag", () => {
    const { title, others } = titleUserIds(C1_CONTACT_KEY_ALICE.userIds);
    // CAPTURED: gpg made "Alice <b>Bold</b> <alice@example.org>"; the server
    // listed it as name "Alice", email "b", primary.
    expect(userIdText(title!)).toBe("Alice <b>");
    expect(others.find((u) => u.revoked)?.email).toBe("alice.old@example.org");
  });
});

describe("your keys' history (#815)", () => {
  it("retired, revoked, and unbound-expired keys are history; a bound expired key is not", () => {
    expect(isPastKey(C1_KEY_REPLACED)).toBe(true);
    expect(isPastKey(C1_KEY_REVOKED)).toBe(true);
    expect(isPastKey(C1_KEY_REPLACEMENT)).toBe(false);
    expect(isPastKey({ ...C1_KEY_NOT_LISTED, expired: true })).toBe(false);
    expect(isPastKey({ ...C1_KEY_NOT_LISTED, expired: true, identityId: null })).toBe(true);
  });

  it("a recorded retirement is a date; an unrecorded one says so and invents nothing", () => {
    expect(retiredFact(C1_KEY_REPLACED)).toEqual({ kind: "on", at: C1_KEY_REPLACED.retiredAt });
    expect(retiredFact(C1_KEY_RETIRED_NOT_RECORDED)).toEqual({ kind: "notRecorded" });
    // An older server that sends no retiredAt at all reads the same way.
    const { retiredAt: _drop, ...older } = C1_KEY_RETIRED_NOT_RECORDED;
    expect(retiredFact(older)).toEqual({ kind: "notRecorded" });
    expect(retiredFact(C1_KEY_REPLACEMENT)).toEqual({ kind: "none" });
  });

  it("a revoke records both at once, so the revoked line alone says it", () => {
    // CAPTURED: retiredAt and revokedAt are the same second.
    expect(C1_KEY_REVOKED.retiredAt).toBe(C1_KEY_REVOKED.revokedAt);
    expect(retiredFact(C1_KEY_REVOKED)).toEqual({ kind: "none" });
    expect(retiredFact({ ...C1_KEY_REVOKED, retiredAt: null })).toEqual({ kind: "none" });
    expect(retiredFact({ ...C1_KEY_REVOKED, retiredAt: "2026-01-02T00:00:00Z" })).toEqual({
      kind: "on",
      at: "2026-01-02T00:00:00Z",
    });
  });
});

describe("correspondents' keys (#815, #818)", () => {
  it("history events land in the closed set; an unknown kind is `other`", () => {
    expect(keyHistoryLines(C1_HISTORY_BOB).map((l) => [l.kind, l.source])).toEqual([
      ["first_seen", "manual"],
      ["refused", "user"],
      ["removed", "user"],
    ]);
    expect(keyHistoryLines([{ kind: "teleported", source: "carrier-pigeon", at: "x", expiresAt: null }])[0]).toEqual({
      kind: "other",
      source: "other",
      at: "x",
      expiresAt: null,
    });
  });

  it("decide is asked about the pinned address, else one the key and the contact share", () => {
    expect(decideAddress(C1_CONTACT_KEY_ALICE, ["alice@example.org"])).toBe("alice@example.org");
    // CAPTURED: bob's pin went with the removal; the contact's address is used.
    expect(decideAddress(C1_CONTACT_KEY_BOB_REMOVED, ["bob@example.org"])).toBe("bob@example.org");
    expect(decideAddress({ pinnedFor: [], addresses: ["b", "x@example.org"] }, ["x@example.org"])).toBe(
      "x@example.org",
    );
  });

  it("compose finds the held key for an address, never a removed one", () => {
    const keys = [C1_CONTACT_KEY_ALICE, C1_CONTACT_KEY_CAROL_EXPIRED, C1_CONTACT_KEY_BOB_REMOVED];
    expect(heldKeyForAddress(keys, C1_CONTACTS, "Carol@Example.org")?.id).toBe(C1_CONTACT_KEY_CAROL_EXPIRED.id);
    expect(heldKeyForAddress(keys, C1_CONTACTS, "bob@example.org")).toBeNull();
    expect(heldKeyForAddress(keys, C1_CONTACTS, "nobody@example.net")).toBeNull();
  });

  it("each refresh result keeps its meaning, and an unknown one is not `unchanged`", () => {
    expect(refreshOutcome(C1_REFRESH_ALICE_NOT_FOUND)).toEqual({ kind: "notFound" });
    expect(refreshOutcome(C1_REFRESH_CAROL_RENEWED)).toEqual({ kind: "renewed", expiresAt: "2028-11-03T18:25:11Z" });
    expect(refreshOutcome(C1_REFRESH_ALICE_UNCHANGED)).toEqual({ kind: "unchanged" });
    expect(refreshOutcome(C1_REFRESH_ALICE_UNREACHABLE)).toEqual({ kind: "unreachable" });
    expect(refreshOutcome(C1_REFRESH_ALICE_DIFFERENT)).toEqual({
      kind: "differentKey",
      newFingerprint: C1_CONTACT_KEY_BOB_REMOVED.fingerprint,
      newKeyId: C1_CONTACT_KEY_BOB_REMOVED.id,
    });
    expect(refreshOutcome({ ...C1_REFRESH_ALICE_NOT_FOUND, result: "somethingNew" })).toEqual({ kind: "unknown" });
  });

  it("checking is prominent when the key expires soon or has expired, and not for a revoked key", () => {
    expect(refreshIsProminent(C1_CONTACT_KEY_ALICE)).toBe(true); // 10 days: expiringSoon
    expect(refreshIsProminent(C1_CONTACT_KEY_CAROL_EXPIRED)).toBe(true);
    expect(refreshIsProminent(C1_CONTACT_KEY_BOB_REMOVED)).toBe(false); // two years
    expect(refreshIsProminent({ ...C1_CONTACT_KEY_CAROL_EXPIRED, revoked: true })).toBe(false);
  });
});

describe("a failed replace (#817)", () => {
  it("names the new key it left behind only when the store step failed after generation", () => {
    expect(replaceLeftNewKey(new JmapMethodError("c", C1_REPLACE_FAILED_AFTER_GENERATION))).toMatch(
      /^[0-9A-HJKMNP-TV-Z]{26}$/,
    );
    // CAPTURED refusals come before anything is generated: nothing left.
    for (const e of [C1_METHOD_ERRORS.replaceRetired, C1_METHOD_ERRORS.replaceUnbound, C1_METHOD_ERRORS.replaceRevoked]) {
      expect(replaceLeftNewKey(new JmapMethodError("c", e)), e.type).toBeNull();
    }
    expect(replaceLeftNewKey(new Error("x"))).toBeNull();
    // An id in some other error type is not read.
    expect(
      replaceLeftNewKey(new JmapMethodError("c", { ...C1_REPLACE_FAILED_AFTER_GENERATION, type: "rateLimited" })),
    ).toBeNull();
  });
});
