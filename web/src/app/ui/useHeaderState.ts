// SPDX-License-Identifier: AGPL-3.0-only

// WHAT THE HEADER SAYS ABOUT THE CONNECTION, ASSEMBLED ONCE (#375 increment 6).
//
// The decision itself is pure and lives in core/headerState.ts. This is only
// the wiring: which store fields and which health fields feed it. It is a hook
// rather than two copies in DeskRoute and TrayRoute for the reason those two
// components already share `HeaderSentence` — a desk that says "Offline" beside
// a tray that says "Sync is behind" about the same minute is the client
// disagreeing with itself, and the way that happens is two call sites drifting.
//
// ⚠ THE MAPPING BELOW IS THE HONEST HALF, and it is where the gaps are visible:
// `AccountSyncStatus` (core/jmap/types.ts) carries `lagSeconds`, `lastError`,
// `lastErrorAt`, `lastSuccessAt`, `lastAttemptAt` and `consecutiveFailures` —
// so the sentence can say how far behind an account is and when it last
// managed to talk to the provider, and it CANNOT say how often Mailroom will
// try again, because nothing reports that. `retryEverySeconds` is therefore
// null here rather than a plausible-looking constant: a client that printed
// "retrying every 5 min" from a number it made up would be inventing a promise
// about the user's mail.

import { useMemo } from "react";

import { headerState } from "../../core/headerState";
import type { HeaderAccount, HeaderState } from "../../core/headerState";
import { useMailState } from "../state";
import { useAccounts } from "./useAccounts";

export function useHeaderState(now: number): HeaderState {
  const state = useMailState();
  const { accounts } = useAccounts();

  const health: HeaderAccount[] = useMemo(
    () =>
      accounts.map((a) => ({
        name: a.name,
        status: a.status,
        // Every one of these is null until the health fetch lands, and
        // `useAccounts` deliberately claims nothing before it does — so an
        // account with no card is `status: "ok"` with a null lag, which
        // `headerState` reads as "not stale". That is right: "we have not asked
        // yet" must not render as an outage.
        lagSeconds: a.health?.syncStatus.lagSeconds ?? null,
        lastError: a.health?.syncStatus.lastError ?? null,
        lastErrorAt: a.health?.syncStatus.lastErrorAt ?? null,
        retryEverySeconds: null,
      })),
    [accounts],
  );

  return useMemo(
    () =>
      headerState(
        {
          conn: state.conn,
          syncError: state.syncError,
          syncErrorKind: state.syncErrorKind,
          queueDepth: state.queue.length,
          lastSyncAt: state.lastSyncAt,
          accounts: health,
        },
        now,
      ),
    [state.conn, state.syncError, state.syncErrorKind, state.queue.length, state.lastSyncAt, health, now],
  );
}
