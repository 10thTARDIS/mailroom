// SPDX-License-Identifier: AGPL-3.0-only

// The desk's follow-up nudges (#58): FollowUp/get, refreshed as mail moves.
//
// FollowUp/get is computed on read (internal/api/follow_up.go), so this asks
// again when something could have changed the answer — a sync landed (a reply
// may have arrived), or the settings moved — but at most once a minute for
// syncs: the answer is a judgement in DAYS, and a request per poll would be
// a server parse loop for a list that changes about once a week.
//
// Done disables the row's buttons until the server has recorded it, then
// takes the row away; a Done the server refuses leaves the row where it was
// (the desk says so in a toast) rather than hiding a nudge that will come
// back. Snooze takes the row away once the snooze is applied.

import { useCallback, useEffect, useRef, useState } from "react";

import type { FollowUpItem, FollowUpSettings, Id } from "../../../core/jmap/types";

/** The least time between two refreshes caused by syncs. */
export const FOLLOW_UP_REFRESH_MS = 60_000;

export interface FollowUpClient {
  followUpGet(): Promise<{ list: FollowUpItem[] }>;
  followUpDismiss(emailId: Id, kind: FollowUpItem["kind"]): Promise<boolean>;
}

export interface FollowUps {
  items: FollowUpItem[];
  pending: ReadonlySet<string>;
  /** Mark one Done. Resolves false when the server did not record it (the
   * row is back in the list by then). */
  dismiss: (item: FollowUpItem) => Promise<boolean>;
  /** Take a row away locally (it was snoozed). */
  remove: (emailId: Id) => void;
}

export function useFollowUps(
  client: FollowUpClient,
  accountId: Id | null,
  settings: FollowUpSettings | undefined,
  lastSyncAt: number | null,
  now: () => number = Date.now,
): FollowUps {
  const [items, setItems] = useState<FollowUpItem[]>([]);
  const [pending, setPending] = useState<ReadonlySet<string>>(new Set());
  const lastFetch = useRef(0);
  // The clock through a ref: a caller passing a fresh arrow each render must
  // not make every effect below re-run (and re-ask the server) on every render.
  const clock = useRef(now);
  clock.current = now;
  const enabled = settings !== undefined && (settings.outbound || settings.inbound);
  const settingsKey = settings ? `${settings.outbound}/${settings.inbound}/${settings.inboundDays}` : "";

  const fetchNow = useCallback(
    (cancelled: () => boolean) => {
      lastFetch.current = clock.current();
      void client
        .followUpGet()
        .then((r) => {
          if (!cancelled()) setItems(r.list);
        })
        .catch(() => {
          // A nudge list that could not be fetched is drawn as nothing, the
          // same as nothing due: it is a reminder, not a record, and the next
          // sync asks again.
        });
    },
    [client],
  );

  // Settings or account changed: ask straight away (or clear when off).
  useEffect(() => {
    let off = false;
    if (!accountId || !enabled) {
      // Only when there is something to clear: a fresh [] every time would be
      // a new state and a re-render for nothing.
      setItems((prev) => (prev.length === 0 ? prev : []));
      return;
    }
    fetchNow(() => off);
    return () => {
      off = true;
    };
  }, [accountId, enabled, settingsKey, fetchNow]);

  // A sync landed: ask again, at most once a minute.
  useEffect(() => {
    if (!accountId || !enabled || lastSyncAt === null) return;
    if (clock.current() - lastFetch.current < FOLLOW_UP_REFRESH_MS) return;
    let off = false;
    fetchNow(() => off);
    return () => {
      off = true;
    };
  }, [lastSyncAt, accountId, enabled, fetchNow]);

  const remove = useCallback((emailId: Id) => {
    setItems((list) => list.filter((i) => i.emailId !== emailId));
  }, []);

  const dismiss = useCallback(
    async (item: FollowUpItem): Promise<boolean> => {
      setPending((p) => new Set(p).add(item.emailId));
      let ok = false;
      try {
        ok = await client.followUpDismiss(item.emailId, item.kind);
      } catch {
        ok = false;
      }
      setPending((p) => {
        const next = new Set(p);
        next.delete(item.emailId);
        return next;
      });
      if (ok) remove(item.emailId);
      return ok;
    },
    [client, remove],
  );

  return { items, pending, dismiss, remove };
}
