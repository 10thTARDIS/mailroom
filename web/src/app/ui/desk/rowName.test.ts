// SPDX-License-Identifier: AGPL-3.0-only

// EVERY CASE HERE IS A PAIR: the mark present, and the same row with it absent.
//
// That is not thoroughness for its own sake. A name assembled from the wrong
// field — `isSeen` for `isFlagged`, `snoozedUntil` for `returnedAt` — produces a
// perfectly plausible sentence, and a test that only ever asserted the loaded
// case would pass on it. The pair is what makes the assertion about the FIELD.

import { describe, expect, it } from "vitest";

import { rowAccessibleName, rowNameFor } from "./rowName";
import type { RowNameEmail } from "./rowName";

/** The plainest row there is: read, unmarked, nothing in flight. */
const plain = {
  unread: false,
  sender: "Ada Lovelace",
  subject: "Your invoice is ready",
  time: "09:14",
};

describe("rowAccessibleName — the row as one sentence", () => {
  it("is sender, subject and time for a row with nothing on it", () => {
    expect(rowAccessibleName(plain)).toBe("Ada Lovelace, Your invoice is ready, 09:14");
  });

  it("says Unread first, and says nothing at all when the row is read", () => {
    expect(rowAccessibleName({ ...plain, unread: true })).toBe(
      "Unread, Ada Lovelace, Your invoice is ready, 09:14",
    );
    // Not "Read": the absence is the statement.
    expect(rowAccessibleName(plain)).not.toContain("Read");
  });

  it("names the area and the alias the message arrived at", () => {
    expect(rowAccessibleName({ ...plain, areaName: "Bills", alias: "invoices@" })).toBe(
      "Ada Lovelace, Your invoice is ready, in Bills, arrived at invoices@, 09:14",
    );
    // No area (Needs sorting, or the tray page which never asked) and no alias:
    // the clauses are gone, not empty.
    expect(rowAccessibleName({ ...plain, areaName: null, alias: "" })).toBe(
      "Ada Lovelace, Your invoice is ready, 09:14",
    );
  });

  it("puts every mark in words, in a fixed order", () => {
    expect(
      rowAccessibleName({
        ...plain,
        flagged: true,
        pinned: true,
        returned: true,
        hasAttachment: true,
        heldAttachment: true,
        warning: "This domain resembles example.com",
        threadCount: 3,
        queued: true,
        pending: true,
      }),
    ).toBe(
      [
        "Ada Lovelace",
        "Your invoice is ready",
        "09:14",
        "In the tray",
        "Pinned",
        "Came back from a snooze",
        "Has an attachment",
        "An attachment is held",
        "This domain resembles example.com",
        "3 messages in this conversation",
        "waiting to send",
        "Change not yet confirmed by the server",
      ].join(", "),
    );
  });

  it("drops each mark independently when its fact is absent", () => {
    // One at a time, because a name built from the wrong field is usually right
    // about the OTHER fields — the interesting failure is the single swap.
    expect(rowAccessibleName({ ...plain, flagged: true })).toContain("In the tray");
    expect(rowAccessibleName({ ...plain, flagged: false })).not.toContain("In the tray");
    expect(rowAccessibleName({ ...plain, pinned: true })).toContain("Pinned");
    expect(rowAccessibleName({ ...plain, pinned: false })).not.toContain("Pinned");
    expect(rowAccessibleName({ ...plain, returned: true })).toContain("Came back from a snooze");
    expect(rowAccessibleName({ ...plain, returned: false })).not.toContain("Came back");
    expect(rowAccessibleName({ ...plain, heldAttachment: true })).toContain("held");
    expect(rowAccessibleName({ ...plain, heldAttachment: false })).not.toContain("held");
    expect(rowAccessibleName({ ...plain, queued: true })).toContain("waiting to send");
    expect(rowAccessibleName({ ...plain, queued: false })).not.toContain("waiting to send");
  });

  it("says nothing about a conversation of one, and counts one of many", () => {
    // `threadCount` is null when the thread is unknown OR holds one message — a
    // "1" on every row would be noise, and a count for a thread nobody loaded
    // would be a guess (ui/desk/DeskRow.tsx).
    expect(rowAccessibleName({ ...plain, threadCount: null })).toBe(
      "Ada Lovelace, Your invoice is ready, 09:14",
    );
    expect(rowAccessibleName({ ...plain, threadCount: 4 })).toContain(
      "4 messages in this conversation",
    );
  });
});

/** A row as the store has it, with only the fields the name reads. */
function email(over: Partial<RowNameEmail> = {}): RowNameEmail {
  return {
    isSeen: true,
    isFlagged: false,
    isPinned: false,
    pending: false,
    returnedAt: null,
    receivedAt: "2026-09-08T09:14:00Z",
    from: [{ name: "Ada Lovelace", email: "ada@example.com" }],
    subject: "Your invoice is ready",
    deliveredTo: "invoices@example.com",
    ...over,
  };
}

const NOW = Date.parse("2026-09-08T10:00:00Z");
const CTX = {
  areaName: "Bills",
  accountName: null,
  threadCount: null,
  queued: false,
  now: NOW,
};

describe("rowNameFor — the same sentence, from a real row", () => {
  it("reads the message's own fields, not their neighbours", () => {
    const name = rowNameFor(email(), CTX);
    expect(name).toContain("Ada Lovelace");
    expect(name).toContain("Your invoice is ready");
    expect(name).toContain("in Bills");
    // The alias is the LOCAL PART with the `@` kept (format.ts aliasOf) — the
    // domain would repeat on every row of a catch-all account.
    expect(name).toContain("arrived at invoices@");
    expect(name).not.toContain("Unread");
  });

  it("says Unread from `isSeen`, and the tray from `isFlagged`", () => {
    expect(rowNameFor(email({ isSeen: false }), CTX)).toContain("Unread");
    expect(rowNameFor(email({ isFlagged: true }), CTX)).toContain("In the tray");
    // Both false on the same fixture, so neither can be reading the other.
    expect(rowNameFor(email(), CTX)).not.toContain("In the tray");
  });

  it("names the account only when the row draws its dot", () => {
    // The dot is a colour and nothing else, so the account is a word on exactly the
    // rows that carry one — and absent, not "unknown", on a single-account desk.
    expect(rowNameFor(email(), { ...CTX, accountName: "Personal" })).toContain(
      "Personal account",
    );
    expect(rowNameFor(email(), CTX)).not.toContain("account");
  });

  it("says a snooze brought it back from `returnedAt`, which is the field the tray ages by", () => {
    expect(rowNameFor(email({ returnedAt: "2026-09-08T08:00:00Z" }), CTX)).toContain(
      "Came back from a snooze",
    );
    expect(rowNameFor(email(), CTX)).not.toContain("Came back");
  });

  it("holds an attachment only when the SERVER quarantined a part", () => {
    const held = email({
      hasAttachment: true,
      attachmentScan: {
        parts: { "2": { state: "flagged", released: false } },
        quarantined: ["2"],
      },
    });
    expect(rowNameFor(held, CTX)).toContain("An attachment is held");
    // A scan with nothing quarantined is a clean attachment: the row says it has
    // one and does NOT say it is held. Re-deriving "held" from `state` here is the
    // mistake ui/attachmentScanPolicy.ts exists to prevent.
    const clean = email({
      hasAttachment: true,
      attachmentScan: { parts: { "2": { state: "flagged", released: true } }, quarantined: [] },
    });
    expect(rowNameFor(clean, CTX)).toContain("Has an attachment");
    expect(rowNameFor(clean, CTX)).not.toContain("held");
  });

  it("carries the sender warning's whole sentence, and nothing when there is none", () => {
    const warned = email({
      senderWarnings: [{ kind: "lookalikeDomain", domain: "exarnple.com", resembles: "example.com" }],
    });
    expect(rowNameFor(warned, CTX)).toContain("exarnple.com");
    // `senderWarnings` is a BODY property: a row fetched as part of a list has
    // none, and the mark is absent rather than invented.
    expect(rowNameFor(email(), CTX)).not.toContain("resembles");
  });
});
