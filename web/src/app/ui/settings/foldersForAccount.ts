// SPDX-License-Identifier: AGPL-3.0-only

// FILTERING THE PER-ACCOUNT FOLDER LIST (#488).
//
// Mailbox now carries `accountId`, so the account row's "Folders synced"
// control can show ONLY this account's folders instead of the whole login's
// list with a caveat (see AccountsRow.tsx's file header for the limitation
// this replaced). Pulled into its own pure function because the wrong half
// of this is easy to get backwards: a folder from ANOTHER account must never
// appear here — folder names collide across accounts, every account has an
// INBOX, a Sent, an Archive — and a folder that legitimately belongs to this
// account must never be dropped just because nothing else matched it.

import type { Id, Mailbox } from "../../../core/jmap/types";

/**
 * The non-roled folders belonging to one account, sorted by name — the same
 * ordering FoldersControl rendered before this filter existed.
 *
 * A mailbox whose `accountId` does not equal `accountId` is excluded;
 * nothing is invented for an account that matches none at all (an account
 * with no non-roled folders yields an empty list, not a guess).
 */
export function foldersForAccount(mailboxes: Mailbox[], accountId: Id): Mailbox[] {
  return mailboxes
    .filter((m) => !m.role && m.accountId === accountId)
    .sort((a, b) => a.name.localeCompare(b.name));
}
