// SPDX-License-Identifier: AGPL-3.0-only

// OFFLINE, AND STALE — WHICH ONE THE HEADER IS ABOUT (#375 increment 6).
// DOM-free and framework-agnostic (spec §12), like the rest of core/.
//
// TWO DIFFERENT TRUTHS THAT LOOK THE SAME ON SCREEN, and the whole reason this
// is a function rather than three booleans in JSX:
//
//   OFFLINE  this BROWSER cannot reach Mailroom. Everything on screen is what
//            was loaded before that happened, and everything you do is being
//            written down here and will be sent when the connection comes back.
//            The number that matters is how much is waiting to send.
//   STALE    Mailroom is perfectly reachable and answering; it is MAILROOM that
//            cannot reach your mail PROVIDER, so the desk is current with a
//            server that is itself behind. Nothing you do is at risk, and the
//            number that matters is how far behind that account is.
//
// Telling a user "offline" when they are not is how you teach them to distrust
// the banner; telling them "sync is behind" when their own connection dropped
// sends them to look at the wrong thing. So the precedence is fixed here, in
// one testable expression, rather than emerging from the order of three `if`s
// in two different components:
//
//   offline  ▸  stale  ▸  error  ▸  ok
//
// Offline first because it EXPLAINS the others: with no connection, of course
// the account health is old and of course the last request failed, and stating
// three consequences of one cause is three sentences for one fact.
//
// ⚠ EVERY CLAUSE IS OPTIONAL BECAUSE THE HEALTH DATA IS. `AccountSyncStatus`
// (core/jmap/types.ts) carries `lagSeconds`, `lastError`, `lastErrorAt`,
// `lastSuccessAt`, `lastAttemptAt` and `consecutiveFailures`, and every one of
// the nullable ones is genuinely null for an account that has never synced. So
// the output carries nulls through rather than substituting zeroes: "last
// synced 0 s ago" for an account that has never synced once is precisely the
// invented number this codebase keeps a rule about (CLAUDE.md).

import type { ConnState, SyncErrorKind } from "./store";

/**
 * How far behind an account may fall before the desk says so out loud.
 *
 * Fifteen minutes, because that is comfortably longer than any normal provider
 * poll cycle — a threshold below it would light the header up every time a
 * fetch ran a minute late, which is how a warning becomes wallpaper.
 */
export const STALE_AFTER_SECONDS = 15 * 60;

/** One account's health, as the header needs it. A narrowed view of
 * `AccountEntry` + `AccountSyncStatus` so this module can be tested without a
 * store, a client or a fixture of the whole JMAP shape. */
export interface HeaderAccount {
  name: string;
  status: "ok" | "paused" | "failing";
  /** Whole seconds since this account's last SUCCESSFUL sync, or null when it
   * has never had one (or when no health has been fetched). */
  lagSeconds?: number | null;
  lastError?: string | null;
  /** RFC 3339 instant of that error, or null. Rendered as a clock time by the
   * layer that has a locale; kept as the raw instant here. */
  lastErrorAt?: string | null;
  /**
   * How often Mailroom retries this account, in seconds.
   *
   * ⚠ NOTHING FILLS THIS TODAY. `Account/getHealth` reports what happened
   * (`lastAttemptAt`, `consecutiveFailures`) but not the cadence it will happen
   * at, so the caller passes null and the "retrying every N" clause is simply
   * absent — which is the honest outcome, not a bug to paper over with a
   * hardcoded interval. It stays in the contract because the sentence the
   * design asks for has that clause and the day the server reports a cadence
   * this is a one-line wiring change, not a redesign.
   */
  retryEverySeconds?: number | null;
}

export interface HeaderStateInput {
  conn: ConnState;
  syncError: string | null;
  /** Whether the last sync failure was the NETWORK or the SERVER saying no.
   * The two mean opposite things to a reader and cannot be told apart from the
   * message text, so the store records which (core/store.ts setSyncError). */
  syncErrorKind: SyncErrorKind | null;
  /** How many mutations are queued and unsent. */
  queueDepth: number;
  /** Wall-clock ms of the last successful round trip, or null. */
  lastSyncAt: number | null;
  accounts: HeaderAccount[];
}

export type HeaderState =
  | { kind: "ok" }
  | { kind: "offline"; lastSyncAt: number | null; queued: number }
  | {
      kind: "stale";
      account: string;
      /** Null when this account has never synced — the clause is then omitted
       * rather than printed as zero. */
      lagSeconds: number | null;
      /** RFC 3339 instant the provider was last reachable at, when there is a
       * recorded error to date it. Absent otherwise. */
      since?: string;
      retryEverySeconds?: number;
    }
  | { kind: "error"; message: string };

/** How bad an account's own state is, for picking the one to name. `failing`
 * outranks `paused` because a paused account is a state the USER chose and a
 * failing one is not; a merely lagging account (status ok, big lag) is last. */
function rank(a: HeaderAccount): number {
  if (a.status === "failing") return 2;
  if (a.status === "paused") return 1;
  return 0;
}

/** True when this account is behind enough to be worth a sentence. */
function isStale(a: HeaderAccount): boolean {
  if (a.status !== "ok") return true;
  return typeof a.lagSeconds === "number" && a.lagSeconds > STALE_AFTER_SECONDS;
}

/**
 * The one thing the header should say about the connection, or `ok`.
 *
 * `now` is not used to DECIDE anything — the lag is already a duration the
 * server measured, and the queue depth is a count — but it is in the signature
 * because the renderer needs a `now` for the same sentence and taking two
 * different ones (one here, one there) is how "synced 0 s ago" appears beside
 * "offline". Passing it through keeps one instant behind one sentence.
 */
export function headerState(input: HeaderStateInput, _now: number): HeaderState {
  // 1. OFFLINE. The connection state is the primary signal — the poll sets it
  //    the moment a round trip fails to happen at all (core/sync.ts). A
  //    TRANSPORT syncError counts too, but only while the connection is not
  //    believed online: a single failed load on an otherwise live connection is
  //    a blip the next poll will clear, and calling that "offline" would flash
  //    the whole header for one dropped request.
  if (
    input.conn === "offline" ||
    (input.syncErrorKind === "transport" && input.syncError !== null && input.conn !== "online")
  ) {
    return { kind: "offline", lastSyncAt: input.lastSyncAt, queued: input.queueDepth };
  }

  // 2. STALE. The worst account when several: by how bad its own state is,
  //    then by how far behind it is. Naming the second-worst would be a
  //    sentence about an account the user is less likely to care about, and
  //    naming "2 accounts" would be a number rather than an answer.
  const worst = input.accounts
    .filter(isStale)
    .sort((a, b) => rank(b) - rank(a) || (b.lagSeconds ?? 0) - (a.lagSeconds ?? 0))[0];
  if (worst) {
    const since = worst.lastError && worst.lastErrorAt ? worst.lastErrorAt : undefined;
    const retry =
      typeof worst.retryEverySeconds === "number" && worst.retryEverySeconds > 0
        ? worst.retryEverySeconds
        : undefined;
    return {
      kind: "stale",
      account: worst.name,
      lagSeconds: typeof worst.lagSeconds === "number" ? worst.lagSeconds : null,
      ...(since ? { since } : {}),
      ...(retry ? { retryEverySeconds: retry } : {}),
    };
  }

  // 3. ERROR. The server answered and said no (or answered something we could
  //    not use). Last, because it is the narrowest: the two states above are
  //    about mail not arriving, and this one is about one request.
  if (input.syncError !== null) return { kind: "error", message: input.syncError };

  return { kind: "ok" };
}
