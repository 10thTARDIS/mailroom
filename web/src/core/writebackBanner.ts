// SPDX-License-Identifier: AGPL-3.0-only

// WHICH ACCOUNTS, IN VIEW, ARE RUNNING INGEST-ONLY RIGHT NOW (ADR-028 §5, #521).
//
// This is deliberately a SEPARATE decision from headerState.ts's offline/
// stale/error/ok, even though the two modules share a shape (a narrow pure
// input, a small output, a hook that wires the store to it). ADR-028 §1
// rejects folding writeback state into anything connectivity-shaped for the
// same reason headerState keeps `sync_enabled` and health apart: they are
// ORTHOGONAL axes. A shadowed account can be perfectly in sync — health
// green, lag zero — and the banner must still say so; a connectivity banner
// competing for the same slot would let "Offline" hide the one fact ADR-028
// says must never go silent.
//
// ⚠ NOTHING IS CLAIMED before `Account/getHealth` answers. `writebackEnabled:
// null` means "the effective flag has not been read yet", and such an account
// is treated as NOT shadowed rather than guessed either way — the same rule
// useAccounts.ts states for `status` before its own fetch lands. Claiming
// "writing normally" from a guess would be inventing the one sentence this
// mode exists to make true.
//
// ⚠ ONE NOTICE PER SHADOWED ACCOUNT, NOT A WORST-OF-MANY. headerState ranks
// accounts because staleness is a severity; ADR-028 §5's contract is a
// boolean fact per account, so a user viewing two accounts at once with both
// shadowed must be told about both — picking the "worse" one would silently
// drop the other's suppressed-changes count, which is exactly the number
// CLAUDE.md's measurement discipline says must never be reported as one
// total when it can be per-case.

import type { Id } from "./jmap/types";

/** One account's writeback facts, narrowed from `AccountHealth.writeback`
 * (core/jmap/types.ts) so this module tests without a store or a JMAP
 * fixture. */
export interface ShadowAccount {
  id: Id;
  name: string;
  /**
   * The EFFECTIVE flag — `AccountHealth.writeback.enabled`, which already
   * ANDs the account's own column with the instance-level switch (ADR-028
   * §2). `null` before that read has landed; never derived from
   * `Account.writebackEnabled` alone, which can read true while this reads
   * false.
   */
  writebackEnabled: boolean | null;
  /** `AccountHealth.writeback.suppressedIntents` — a durable ledger (ADR-028
   * §4), so this does not fall to zero merely because the mode lifts. */
  suppressedIntents: number;
}

/** What the banner states about one shadowed account. */
export interface ShadowNotice {
  id: Id;
  name: string;
  suppressedIntents: number;
}

/**
 * Every account IN VIEW that is shadowed right now.
 *
 * `scope` narrows to one account the way the desk's own scope does
 * (`ui/routes/DeskRoute.tsx`'s `scope`); `null` means "every account", the
 * desk's unscoped view — so an account with no identity synced into view yet
 * simply is not in the input list, the same boundary `useAccounts.ts` draws.
 */
export function shadowNotices(accounts: ShadowAccount[], scope: Id | null): ShadowNotice[] {
  return accounts
    .filter((a) => scope === null || a.id === scope)
    .filter((a) => a.writebackEnabled === false)
    .map((a) => ({ id: a.id, name: a.name, suppressedIntents: a.suppressedIntents }));
}
