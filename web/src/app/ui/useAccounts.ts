// SPDX-License-Identifier: AGPL-3.0-only

// WHO THE USER'S MAIL ACCOUNTS ARE, AND HOW EACH ONE IS DOING (#375).
//
// Three surfaces need the same answer and used to compute three versions of it:
// the rail's account switcher (which account is selected, what its dot looks
// like, whether any account is paused or failing), the desk row's account dot,
// and the account-health panel. They must agree — a row dotted in one colour
// under a switcher line that names another account is one account described
// twice — so the derivation lives here, once.
//
// TWO SOURCES, AND THE DIVISION BETWEEN THEM IS DELIBERATE:
//
//   * WHICH ACCOUNTS EXIST comes from the SYNCED store — Identity.accountId is
//     the only place JMAP exposes an account id per message (core/views.ts) —
//     so the list is there from the first render, offline, and cannot vanish
//     because a telemetry call failed. It is UNIONED with the accounts
//     `Account/get` returned (#662): an account added ingest-only has no
//     identity at all, and an identity-only list never showed it, so its
//     controls (pause, Writeback) were unreachable.
//   * WHAT STATE EACH ONE IS IN is fetched: `Account/get` for `syncEnabled`
//     (the real "this account is paused" flag) and `Account/getHealth` for the
//     sync telemetry. Neither is a synced object — health is computed on demand
//     — so this fetches once per session into a module-level cache and hands
//     the same snapshot to every caller, rather than one request per component
//     that wants a dot.
//
// UNTIL THE FETCH LANDS, NOTHING IS CLAIMED. An account whose record has not
// arrived is neither paused nor failing and gets an ordinary colour, because
// "paused" is a statement about the user's mail not arriving and a client that
// guessed it would be inventing an outage. This is what replaced the stand-in
// increment 3 used — "no identity for this account is enabled" — which is a
// different fact that merely correlated with the one being drawn.

import { useCallback, useEffect, useMemo, useSyncExternalStore } from "react";

import type { Account, AccountHealth, Id, OperatorNotice } from "../../core/jmap/types";
import type { JmapClient } from "../../core/jmap/client";
import { useApp, useMailState } from "../state";
import type { AccountTone } from "./desk/DeskRow";
import { accountColorOrToken } from "./settings/accountColor";

/** How an account is doing, in one word — what the switcher prints beside a
 * name and summarises above the list. `warn` (bounces, dead letters) is
 * deliberately not one of them: it is real, but it is not a reason the mail
 * stopped, and the health cards below the list state it in full. */
export type AccountStatus = "ok" | "paused" | "failing";

export interface AccountEntry {
  id: Id;
  /** The account's own name when the record has loaded, else a representative
   * identity's address — which is what the rail showed before there was one. */
  name: string;
  tone: AccountTone;
  /**
   * The user's chosen `#RRGGBB` tint for this account's dot (#374), or null for
   * "the tone is the answer".
   *
   * ⚠ NULL FOR A PAUSED ACCOUNT WHATEVER IT HOLDS. `--acct-off` is how the rail
   * says "not syncing", and a tint that outranked it would leave the dot claiming
   * the account is live — settings/accountColor.ts owns that decision and says
   * more about it.
   */
  color: string | null;
  status: AccountStatus;
  /** The health card for this account, when it has been fetched. */
  health: AccountHealth | null;
  /** The account record itself, when `Account/get` has answered — the hosts, the
   * ports, the provider profile, `syncEnabled`. Null before it lands, which is
   * why nothing above is claimed from it until then. The settings page's Accounts
   * row is what needs it; the rail and the desk want only the four fields above.
   */
  record: Account | null;
  /** Whether any synced Identity carries this account's id — false for an account
   * added ingest-only (#662), which is listed from its `Account/get` record and
   * has no sending address yet. */
  hasIdentity: boolean;
}

export interface AccountsSnapshot {
  /** Every account, in one stable order: by id. Ascending ULIDs are ascending
   * creation time, so this is "oldest account first" and it does not reshuffle
   * when an identity is added — which matters because `g 1`–`g 9` name accounts
   * BY POSITION, and a position that moved would be a shortcut that switched to
   * a different account than it did yesterday. */
  accounts: AccountEntry[];
  byId: Map<Id, AccountEntry>;
  /**
   * EVERY health card the server returned, in its order — not one per entry
   * above.
   *
   * The two lists can still differ. `accounts` is the union of Identities and
   * Account/get (#662), so an account with no identity now has an entry once
   * Account/get answers; before it does, the entries are identity-only. Rendering
   * the health from the entries would then silently omit the card for an account
   * just added, which is the one most likely to be in trouble.
   */
  healthCards: AccountHealth[];
  /** How many are paused / failing — the switcher's muted trailing clause. */
  paused: number;
  failing: number;
  loading: boolean;
  error: string | null;
  /** Whether the fetched half has ever landed. Before it has, no account is
   * described as paused or failing. */
  loaded: boolean;
  /** Envelope-level, not per-account: how far behind the change log the user's
   * one search index is (spec §7), or undefined when this server has no search
   * backend installed. */
  searchStalenessSeconds: number | undefined;
  /** Admins only (#52): providers refusing this server's sign-in client.
   * undefined for everyone else, and on a server without sign-in providers. */
  operatorNotices: OperatorNotice[] | undefined;
  /**
   * Re-fetch the telemetry — health is a snapshot, so the panel refreshes it
   * when it is opened.
   *
   * AWAITABLE since #374, because a settings control that pauses an account has
   * to wait for the server's own answer before it stops saying "pausing…": the
   * write itself returns a bare success, and this is the only thing that reads
   * back what the account now IS. It never rejects (loadAccounts records failure
   * in the snapshot), so awaiting it cannot throw into a click handler.
   *
   * ⚠ A CALL WHILE ONE IS IN FLIGHT JOINS THAT ONE rather than starting a second
   * (loadAccounts shares its promise). Awaiting it after a write is therefore only
   * as fresh as that in-flight request — fine here, because the write completes
   * before this is called, but not a general "read-after-write" guarantee.
   */
  reload: () => Promise<void>;
}

// -- the module-level cache --------------------------------------------------
//
// A tiny store rather than component state, for the reason the comment at the
// top gives: three components want this and there must be one request. Same
// subscribe/getSnapshot contract the client-core uses, so useSyncExternalStore
// consumes it directly.

interface Fetched {
  accounts: Record<Id, Account> | null;
  health: Record<Id, AccountHealth> | null;
  /** The same cards, in the order the server listed them. */
  healthList: AccountHealth[];
  searchStalenessSeconds: number | undefined;
  operatorNotices: OperatorNotice[] | undefined;
  loading: boolean;
  error: string | null;
}

const EMPTY: Fetched = {
  accounts: null,
  health: null,
  healthList: [],
  searchStalenessSeconds: undefined,
  operatorNotices: undefined,
  loading: false,
  error: null,
};

let fetched: Fetched = EMPTY;
let inFlight: Promise<void> | null = null;
let everRequested = false;
const listeners = new Set<() => void>();

function set(next: Partial<Fetched>): void {
  fetched = { ...fetched, ...next };
  for (const listener of [...listeners]) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getFetched(): Fetched {
  return fetched;
}

/**
 * Fetches both records in ONE batch.
 *
 * Concurrent callers share the in-flight promise: the rail, the desk and the
 * panel all mount within a frame of each other, and three identical requests
 * would be three round trips to draw one dot. A failure is recorded and NOT
 * retried on a timer — this is telemetry, and a client that re-asked every few
 * seconds because the server said no would be a client hammering a server that
 * is already unwell.
 *
 * ⚠ THE ACCOUNT ID IS PASSED IN, NOT READ OFF THE CLIENT. `client.accountId`
 * THROWS before the session has landed, and the rail mounts before the boot
 * sequence connects (a child's effects run before its parent's). Reading it
 * here would therefore throw on every cold start, be swallowed by the catch
 * below, and record a permanent error for a request that was never made — the
 * caller waits for the store's own account id instead, which is set from the
 * session.
 */
async function loadAccounts(client: JmapClient, accountId: string): Promise<void> {
  if (inFlight) return inFlight;
  everRequested = true;
  set({ loading: true, error: null });
  inFlight = (async () => {
    try {
      const [rec, health] = await client.request([
        { name: "Account/get", args: { accountId }, id: "a" },
        { name: "Account/getHealth", args: { accountId }, id: "h" },
      ]);
      // The two are read INDEPENDENTLY. An older server, or one with the
      // accounts capability switched off, can answer one and refuse the other,
      // and a switcher that showed nothing because the health call failed would
      // have lost the account list over a number nobody asked for.
      const accounts: Record<Id, Account> | null = rec?.error
        ? null
        : Object.fromEntries(
            ((rec?.value as { list?: Account[] } | null)?.list ?? []).map((a) => [a.id, a]),
          );
      const cards = health?.error
        ? null
        : ((health?.value as { list?: AccountHealth[] } | null)?.list ?? []);
      set({
        accounts,
        health: cards === null ? null : Object.fromEntries(cards.map((h) => [h.id, h])),
        healthList: cards ?? [],
        searchStalenessSeconds: health?.error
          ? undefined
          : (health?.value as { searchStalenessSeconds?: number } | null)
              ?.searchStalenessSeconds,
        operatorNotices: health?.error
          ? undefined
          : (health?.value as { operatorNotices?: OperatorNotice[] } | null)?.operatorNotices,
        loading: false,
        error:
          rec?.error && health?.error ? (rec.error.message ?? "could not read the accounts") : null,
      });
    } catch (err) {
      set({ loading: false, error: err instanceof Error ? err.message : String(err) });
    } finally {
      inFlight = null;
    }
  })();
  return inFlight;
}

/** Drops the cache — exported for tests and for a future account add/remove,
 * which must not leave a switcher listing an account that is gone. */
export function forgetAccounts(): void {
  fetched = EMPTY;
  everRequested = false;
  for (const listener of [...listeners]) listener();
}

/**
 * One entry per account, named by a representative identity — the derivation
 * the rail has always used, kept because Identity is the only synced object
 * that carries an account id.
 *
 * A concrete address beats a `*@domain` wildcard as the display name: a
 * catch-all account whose first identity happens to be the wildcard would
 * otherwise be called "*@example.com" in a list whose whole job is telling two
 * accounts apart.
 */
function identityNames(
  identities: Record<Id, { accountId: string; email: string }>,
): Map<Id, string> {
  const byAccount = new Map<Id, string>();
  for (const identity of Object.values(identities)) {
    if (!identity.accountId) continue;
    const current = byAccount.get(identity.accountId);
    if (!current || (current.startsWith("*@") && !identity.email.startsWith("*@"))) {
      byAccount.set(identity.accountId, identity.email);
    }
  }
  return byAccount;
}

export function useAccounts(): AccountsSnapshot {
  const { client } = useApp();
  const state = useMailState();
  const remote = useSyncExternalStore(subscribe, getFetched, getFetched);

  // Fetched once per session, on whichever surface mounts first, and only once
  // the session exists — the same `state.accountId` guard DeskRoute uses before
  // it loads anything. Not on every mount: the rail is always mounted, so "on
  // mount" would be "on every reload of the app", which is exactly right, while
  // a per-component fetch would be three requests for one answer.
  const accountId = state.accountId;
  useEffect(() => {
    if (!accountId || everRequested) return;
    void loadAccounts(client, accountId);
  }, [client, accountId]);

  const reload = useCallback(async () => {
    if (!accountId) return;
    await loadAccounts(client, accountId);
  }, [client, accountId]);

  return useMemo(() => {
    const names = identityNames(state.identities);
    const ids = [...new Set([...names.keys(), ...Object.keys(remote.accounts ?? {})])].sort(
      (a, b) => a.localeCompare(b),
    );
    const loaded = remote.accounts !== null || remote.health !== null;
    const accounts: AccountEntry[] = ids.map((id, i) => {
      const record = remote.accounts?.[id] ?? null;
      const health = remote.health?.[id] ?? null;
      // PAUSED WINS OVER FAILING. An account whose sync is switched off is not
      // failing, it is off — and its last error is from before it was switched
      // off, so reporting that would be describing a state the user ended.
      const paused = record !== null && !record.syncEnabled;
      // An OAuth account whose sign-in is not working is not arriving either
      // (#52): needs_reauth is skipped without a dial, so it has no fresh
      // error to show, and operator_error fails every pass.
      const signInBroken =
        record !== null && (record.authState === "needs_reauth" || record.authState === "operator_error");
      const failing =
        !paused &&
        (signInBroken ||
          (health !== null &&
            (health.syncStatus.consecutiveFailures > 0 || health.syncStatus.lastError !== null)));
      // The dot's tone AND its chosen colour, from one pure decision (#374) — the
      // ordinal cycle used to live here, and it moved so that "paused beats a
      // chosen colour" could be a unit test rather than a line in a hook.
      const dot = accountColorOrToken({ color: record?.color ?? null, paused }, i);
      return {
        id,
        name: record?.name || names.get(id) || id,
        tone: dot.tone,
        color: dot.color,
        status: paused ? "paused" : failing ? "failing" : "ok",
        health,
        record,
        hasIdentity: names.has(id),
      };
    });
    return {
      accounts,
      byId: new Map(accounts.map((a) => [a.id, a])),
      healthCards: remote.healthList,
      paused: accounts.filter((a) => a.status === "paused").length,
      failing: accounts.filter((a) => a.status === "failing").length,
      loading: remote.loading,
      error: remote.error,
      loaded,
      searchStalenessSeconds: remote.searchStalenessSeconds,
      operatorNotices: remote.operatorNotices,
      reload,
    };
  }, [state.identities, remote, reload]);
}
