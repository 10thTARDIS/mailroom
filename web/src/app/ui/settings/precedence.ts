// SPDX-License-Identifier: AGPL-3.0-only

// IDENTITIES AS A PRECEDENCE, NOT AN INVENTORY (#374 increment 2).
//
// The old snippets sheet listed identities as a set of options in a dropdown,
// which is exactly the wrong shape for the question a user actually has. Under
// spec §5's alias model an account can hold several concrete addresses AND a
// `*@domain` catch-all, and the one thing a list cannot tell you is WHICH ONE a
// new message will be sent from. That is an ORDER, so this renders an order.
//
// THE RULE, in one sentence: a concrete address you can send from beats a
// catch-all that merely accepts, and anything switched off loses to everything
// switched on.
//
//   1. concrete + enabled   → `primary`    the address a reply actually goes out as
//   2. wildcard + enabled   → `catch-all`  accepts mail at the domain; a fallback
//   3. anything disabled    → `disabled`   cannot send at all, concrete or not
//
// DISABLED LOSES LAST REGARDLESS OF SHAPE, and that ordering is the honest one:
// a disabled concrete identity is not "better" than a working catch-all, because
// it cannot send. Ranking it above one would draw a precedence the server would
// not honour.
//
// GROUPED BY ACCOUNT, because precedence is a fact WITHIN an account — an
// identity submits through its own account's SMTP (`Identity.accountId`), so
// there is no ordering between two accounts' identities to state. The groups
// themselves are ordered by account id, which is ascending ULID and therefore
// oldest account first — the same order ui/useAccounts.ts puts accounts in, so
// the Identities row and the account switcher never disagree about which account
// is "first".
//
// PURE and store-free: it takes identities and returns ordered groups. The row
// that renders it supplies the account NAMES, because those come from
// useAccounts and this module must stay testable without one.

import type { Id, Identity } from "../../../core/jmap/types";

/** Why an identity sits where it does — the word printed beside it. Three words,
 * not a sentence: the row is a list, and a reason that needed a clause would be
 * a reason the ordering had not actually decided. */
export type PrecedenceReason = "primary" | "catch-all" | "disabled";

export interface PrecedenceEntry {
  id: Id;
  email: string;
  /** The identity's display name, which may be empty — the row falls back to the
   * address, it does not invent one. */
  name: string;
  reason: PrecedenceReason;
}

export interface PrecedenceGroup {
  accountId: Id;
  entries: PrecedenceEntry[];
}

/** The three ranks, as numbers, so the sort is a single comparison. */
function rankOf(identity: Identity): 0 | 1 | 2 {
  if (!identity.enabled) return 2;
  return identity.isWildcard ? 1 : 0;
}

function reasonOf(identity: Identity): PrecedenceReason {
  const rank = rankOf(identity);
  return rank === 2 ? "disabled" : rank === 1 ? "catch-all" : "primary";
}

/**
 * The identities of every account, in the order mail is actually sent from
 * them, grouped by account.
 *
 * ⚠ `isWildcard` IS THE AUTHORITY ON CATCH-ALL, not the `*@` prefix on the
 * address. The address form is how a wildcard is DISPLAYED (and other surfaces
 * do sniff it — ui/SnippetsSheet.tsx filters on the prefix to decide what a
 * signature can bind to); the boolean is what the server stores and what the
 * submission path consults. Reading the string instead would be a second
 * opinion about the same fact, and the two would eventually disagree on an
 * address that merely began with a star.
 *
 * Ties inside a rank break on the address, so the same set of identities always
 * produces the same order — an order that reshuffled between renders would read
 * as the precedence changing.
 */
export function identityPrecedence(
  identities: Readonly<Record<Id, Identity>> | readonly Identity[],
): PrecedenceGroup[] {
  const all = Array.isArray(identities)
    ? [...(identities as readonly Identity[])]
    : Object.values(identities as Readonly<Record<Id, Identity>>);

  const byAccount = new Map<Id, Identity[]>();
  for (const identity of all) {
    // An identity with no account cannot be placed in a precedence — there is no
    // SMTP path it belongs to — so it is left out rather than filed under "".
    // Dropping it is visible (the row's count comes from the same groups), which
    // is the failure direction that gets noticed.
    if (!identity.accountId) continue;
    const list = byAccount.get(identity.accountId);
    if (list) list.push(identity);
    else byAccount.set(identity.accountId, [identity]);
  }

  return [...byAccount.keys()]
    .sort((a, b) => a.localeCompare(b))
    .map((accountId) => ({
      accountId,
      entries: (byAccount.get(accountId) ?? [])
        .slice()
        .sort((a, b) => rankOf(a) - rankOf(b) || a.email.localeCompare(b.email))
        .map((identity) => ({
          id: identity.id,
          email: identity.email,
          name: identity.name,
          reason: reasonOf(identity),
        })),
    }));
}
