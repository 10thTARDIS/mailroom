// SPDX-License-Identifier: AGPL-3.0-only

// groupFoldersByAccount (#488): the rail's Folders disclosure groups by
// account only when there is more than one, in account order, and never
// invents a home for a folder whose account it does not recognise.

import { describe, expect, it } from "vitest";

import { groupFoldersByAccount } from "./folderGroups";
import type { Mailbox } from "./jmap/types";

function folder(id: string, accountId: string, name: string): Mailbox {
  return { id, accountId, name, role: null } as Mailbox;
}

describe("groupFoldersByAccount", () => {
  it("returns no headings for one account", () => {
    const bulk = folder("m1", "a1", "Bulk");
    const groups = groupFoldersByAccount([bulk], [{ id: "a1", name: "Work" }]);
    expect(groups).toEqual([{ accountId: "a1", name: null, folders: [bulk] }]);
  });

  it("returns nothing for one account with no folders", () => {
    expect(groupFoldersByAccount([], [{ id: "a1", name: "Work" }])).toEqual([]);
  });

  it("returns two groups in account order for two accounts", () => {
    const workBulk = folder("m1", "a1", "Bulk");
    const homeBulk = folder("m2", "a2", "Bulk");
    const groups = groupFoldersByAccount(
      [homeBulk, workBulk], // deliberately out of account order
      [
        { id: "a1", name: "Work" },
        { id: "a2", name: "Home" },
      ],
    );
    expect(groups).toEqual([
      { accountId: "a1", name: "Work", folders: [workBulk] },
      { accountId: "a2", name: "Home", folders: [homeBulk] },
    ]);
  });

  it("omits an account with no non-roled folders rather than an empty heading", () => {
    const workBulk = folder("m1", "a1", "Bulk");
    const groups = groupFoldersByAccount(
      [workBulk],
      [
        { id: "a1", name: "Work" },
        { id: "a2", name: "Home" },
      ],
    );
    expect(groups).toEqual([{ accountId: "a1", name: "Work", folders: [workBulk] }]);
  });

  it("drops a folder whose account is unknown rather than inventing a group for it", () => {
    const workBulk = folder("m1", "a1", "Bulk");
    const orphan = folder("m2", "ghost", "Mystery");
    const groups = groupFoldersByAccount(
      [workBulk, orphan],
      [
        { id: "a1", name: "Work" },
        { id: "a2", name: "Home" },
      ],
    );
    expect(groups).toEqual([{ accountId: "a1", name: "Work", folders: [workBulk] }]);
  });
});
