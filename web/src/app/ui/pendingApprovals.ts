// SPDX-License-Identifier: AGPL-3.0-only

// HOW MANY AGENT REQUESTS ARE WAITING FOR THE USER (#49, ADR-042 §7).
//
// Approval requests are not JMAP-synced objects (no change rows, no state in
// the event stream; ADR-042 §1 says U polls), so this asks PendingAction/get:
//
//   * when the app has a session, and again every POLL_MS while the tab is
//     visible — never while it is hidden;
//   * the moment the tab becomes visible again (the push notification that
//     brought the user back is exactly that moment);
//   * right after the Settings page decides a request (`refresh()`), so the
//     banner and the list cannot disagree for a minute.
//
// ONE request for every consumer: the banner and the Settings list read the
// same module-level snapshot (the useAccounts.ts pattern), keyed by client.
//
// A server without the capability is never asked, and its snapshot stays
// `null` — "cannot say", which renders as nothing, never as "0 waiting".

import { useEffect, useSyncExternalStore } from "react";

import type { JmapClient } from "../../core/jmap/client";
import type { PendingAction } from "../../core/jmap/types";
import { CAP_APIKEYS } from "../../core/jmap/types";
import { useApp, useMailState } from "../state";

/** A modest interval: an approval waits up to 24 hours, and the push brings the
 * user back sooner than any poll would. */
export const POLL_MS = 60_000;

export interface PendingSnapshot {
  /** Requests still waiting (state "pending"), oldest first; null until asked
   * or when this server cannot be asked. */
  waiting: PendingAction[] | null;
}

interface Store {
  snapshot: PendingSnapshot;
  listeners: Set<() => void>;
  inFlight: Promise<void> | null;
}

const stores = new WeakMap<object, Store>();

function storeFor(client: object): Store {
  let s = stores.get(client);
  if (!s) {
    s = { snapshot: { waiting: null }, listeners: new Set(), inFlight: null };
    stores.set(client, s);
  }
  return s;
}

/** Read now. A call while one is in flight joins it. Never rejects. */
export function refreshPending(client: Pick<JmapClient, "pendingActionsGet" | "hasCapability">): Promise<void> {
  const s = storeFor(client);
  if (s.inFlight) return s.inFlight;
  s.inFlight = (async () => {
    try {
      const list = await client.pendingActionsGet(["pending"]);
      // A failed read keeps the last answer rather than claiming "none".
      if (list !== null) {
        const waiting = list
          .filter((p) => p.state === "pending")
          .sort((a, b) => a.createdAt.localeCompare(b.createdAt));
        s.snapshot = { waiting };
        for (const l of s.listeners) l();
      }
    } finally {
      s.inFlight = null;
    }
  })();
  return s.inFlight;
}

/**
 * The waiting requests, polled as described above. `poll` is false for a
 * consumer that only reads (the Settings list piggybacks on the banner's poll
 * and calls refresh itself).
 */
export function usePendingApprovals(poll = true): PendingSnapshot & { refresh: () => Promise<void> } {
  const { client } = useApp();
  const state = useMailState();
  const s = storeFor(client);
  const snapshot = useSyncExternalStore(
    (l) => {
      s.listeners.add(l);
      return () => s.listeners.delete(l);
    },
    () => s.snapshot,
    () => s.snapshot,
  );
  const accountId = state.accountId;
  useEffect(() => {
    if (!poll || !accountId || !client.hasCapability(CAP_APIKEYS)) return;
    const tick = (): void => {
      if (document.visibilityState === "visible") void refreshPending(client);
    };
    tick();
    const timer = setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", tick);
    };
  }, [client, accountId, poll]);
  return { ...snapshot, refresh: () => refreshPending(client) };
}
