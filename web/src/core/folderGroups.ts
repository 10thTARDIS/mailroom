// SPDX-License-Identifier: AGPL-3.0-only

// GROUPING THE RAIL'S FOLDER DISCLOSURE BY ACCOUNT (#488).
//
// Folder NAMES collide across accounts — every account has its own INBOX,
// Sent, Archive — and Mailbox now carries `accountId` (#488), so the rail's
// Folders disclosure can tell one account's non-roled folders from another's
// instead of running them all together under one flat list. With exactly one
// account there is nothing to distinguish, so the rail renders exactly what
// it rendered before this existed: one flat list, no headings.

import type { Id, Mailbox } from "./jmap/types";

/** One named group of folders, in the order it should render. `name` is null
 * for the "no heading" case — the caller (Sidebar.tsx) renders a heading only
 * when `name` is not null, which keeps the single-account rail identical to
 * what it drew before grouping existed. */
export interface FolderGroup {
  accountId: Id | null;
  name: string | null;
  folders: Mailbox[];
}

/**
 * Groups folders by the account that owns them, in the order `accounts`
 * lists them (useAccounts.ts: oldest account first). Empty groups are
 * omitted rather than rendered as an empty heading.
 *
 * ⚠ FEWER THAN TWO ACCOUNTS MEANS NO HEADINGS, not one group with a heading —
 * with a single account there is nothing to tell apart, so introducing a
 * heading here would be a rail that suddenly names things it never used to,
 * for no reader benefit.
 *
 * ⚠ A FOLDER WHOSE accountId MATCHES NONE OF `accounts` IS DROPPED, not
 * invented a home. There is no "unknown account" wording anywhere in the
 * product (checked i18n.ts before writing this) to head such a group with,
 * and a heading that guessed which account a folder belonged to would be
 * exactly the unmeasured claim #488 was opened to stop the client making.
 * This should not happen in practice — every mailbox's accountId names a
 * real account — so losing such a folder from the disclosure is an acceptable
 * (and visible, via its absence) fallback rather than a silent one.
 */
export function groupFoldersByAccount(
  folders: Mailbox[],
  accounts: { id: Id; name: string }[],
): FolderGroup[] {
  if (accounts.length < 2) {
    if (folders.length === 0) return [];
    return [{ accountId: accounts[0]?.id ?? null, name: null, folders }];
  }
  const byAccount = new Map<Id, Mailbox[]>();
  for (const f of folders) {
    const list = byAccount.get(f.accountId);
    if (list) list.push(f);
    else byAccount.set(f.accountId, [f]);
  }
  const groups: FolderGroup[] = [];
  for (const a of accounts) {
    const list = byAccount.get(a.id);
    if (list && list.length > 0) groups.push({ accountId: a.id, name: a.name, folders: list });
  }
  return groups;
}
