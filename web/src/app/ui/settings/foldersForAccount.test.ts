// SPDX-License-Identifier: AGPL-3.0-only

// foldersForAccount (#488): the account row's "Folders synced" control must
// show ONLY the account it sits inside — never a mailbox belonging to another
// account, and never a folder invented for one that has no match at all.

import { describe, expect, it } from "vitest";

import { foldersForAccount } from "./foldersForAccount";
import type { Mailbox } from "../../../core/jmap/types";

const ACC_A = "01JBQZ3K7WX2N4Y6P8R0S1T2A0";
const ACC_B = "01JBQZ3K7WX2N4Y6P8R0S1T2B0";

/** A Mailbox in the shape the server serves, with only the fields
 * foldersForAccount reads varied. */
function mailbox(over: Partial<Mailbox> & Pick<Mailbox, "id" | "accountId" | "name">): Mailbox {
  return {
    parentId: null,
    role: null,
    sortOrder: 0,
    totalEmails: 0,
    unreadEmails: 0,
    totalThreads: 0,
    unreadThreads: 0,
    isSubscribed: true,
    ...over,
  };
}

describe("foldersForAccount", () => {
  it("excludes a mailbox from another account", () => {
    const mine = mailbox({ id: "m1", accountId: ACC_A, name: "Bulk" });
    const theirs = mailbox({ id: "m2", accountId: ACC_B, name: "Bulk" });
    expect(foldersForAccount([mine, theirs], ACC_A)).toEqual([mine]);
  });

  it("does not invent a folder for an account with no match", () => {
    const theirs = mailbox({ id: "m1", accountId: ACC_B, name: "Bulk" });
    expect(foldersForAccount([theirs], ACC_A)).toEqual([]);
  });

  it("excludes roled folders and sorts the rest by name", () => {
    const inbox = mailbox({ id: "m1", accountId: ACC_A, name: "Inbox", role: "inbox" });
    const zeta = mailbox({ id: "m2", accountId: ACC_A, name: "Zeta" });
    const alpha = mailbox({ id: "m3", accountId: ACC_A, name: "Alpha" });
    expect(foldersForAccount([inbox, zeta, alpha], ACC_A)).toEqual([alpha, zeta]);
  });
});
