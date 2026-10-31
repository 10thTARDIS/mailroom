// SPDX-License-Identifier: AGPL-3.0-only

// SENDERS (#59): the people the user has marked — VIPs and blocked senders.
//
// SINCE #816 THE LISTS LIVE ON THE CONTACTS PAGE, as its "VIP" and "Blocked
// senders" filters, with the way back from each on the contact's own page, and
// "mark an address" moved there as "Add a contact by address…". This row keeps
// its state line (how many of each, read here so the settings search still
// finds a marked address) and links to the two filters.
//
// THE COUNTS ARE THE SERVER'S `total`, never a page's length (CLAUDE.md: a
// bounded result reports its bound).

import { useCallback, useEffect, useState } from "react";
import type { JSX } from "react";

import type { Contact } from "../../../../core/jmap/types";
import { formatRoute } from "../../../../core/routes";
import { t } from "../../../i18n";
import { navigate } from "../../../routing";
import { useApp, useMailState } from "../../../state";

export interface MarkedSenders {
  /** null until read, or when the server cannot answer (before #59). */
  vips: { list: Contact[]; total: number } | null;
  blocked: { list: Contact[]; total: number } | null;
  reload: () => void;
}

export function useMarkedSenders(): MarkedSenders {
  const { client } = useApp();
  const accountId = useMailState().accountId;
  const [vips, setVips] = useState<MarkedSenders["vips"]>(null);
  const [blocked, setBlocked] = useState<MarkedSenders["blocked"]>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!accountId) return;
    let cancelled = false;
    void Promise.all([client.contactsMarked({ isVip: true }), client.contactsMarked({ isBlocked: true })])
      .then(([v, b]) => {
        if (cancelled) return;
        // A server before #59 ignores the filters (unknown keys) and returns
        // contacts that carry no markers at all: that is "cannot say", not a
        // list of VIPs.
        const marked = (r: { list: Contact[] }, key: "isVip" | "isBlocked"): boolean =>
          r.list.every((c) => c[key] === true);
        setVips(marked(v, "isVip") ? v : null);
        setBlocked(marked(b, "isBlocked") ? b : null);
      })
      .catch(() => {
        if (!cancelled) {
          setVips(null);
          setBlocked(null);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [client, accountId, tick]);
  const reload = useCallback(() => setTick((n) => n + 1), []);
  return { vips, blocked, reload };
}

/** `2 VIPs · 1 blocked`, each clause omitted until its list has been read. */
export function sendersFacts(s: MarkedSenders): string[] {
  const out: string[] = [];
  if (s.vips) out.push(t(s.vips.total === 1 ? "settings.senders.vipCount.one" : "settings.senders.vipCount.other", { count: s.vips.total }));
  if (s.blocked) out.push(t("settings.senders.blockedCount", { count: s.blocked.total }));
  return out;
}


/** Settings › Senders is a pointer now (#816): the VIP and blocked lists live
 * on the Contacts page, as its VIP and Blocked senders filters, and "mark an
 * address" moved there as "Add a contact by address…". Links, not buttons:
 * they go somewhere, and each one has a URL. */
export function SendersBody({ senders }: { senders: MarkedSenders }): JSX.Element {
  const go = (filter: "vip" | "blocked") => (ev: React.MouseEvent): void => {
    if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    ev.preventDefault();
    navigate({ page: "contacts", filter });
  };
  const count = (n: number | undefined): string => (n === undefined ? "" : ` (${n})`);
  return (
    <>
      <p>{t("settings.senders.moved")}</p>
      <ul className="senders-links">
        <li>
          <a href={formatRoute({ page: "contacts", filter: "vip" })} onClick={go("vip")}>
            {t("settings.senders.vips")}
            {count(senders.vips?.total)}
          </a>
        </li>
        <li>
          <a href={formatRoute({ page: "contacts", filter: "blocked" })} onClick={go("blocked")}>
            {t("settings.senders.blocked")}
            {count(senders.blocked?.total)}
          </a>
        </li>
      </ul>
      <p className="field-hint">{t("settings.senders.hint")}</p>
    </>
  );
}
