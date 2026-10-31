// SPDX-License-Identifier: AGPL-3.0-only

// What the Contacts page asks of the server (#816), as one small interface, so
// the page renders against a fake in its tests and against the JMAP client
// here — the shape Settings → Keys uses (settings/keys/api.ts). Every call
// resolves to an outcome whose refusal is in core/contacts.ts's closed set,
// never a raw error, and never throws.

import { useMemo } from "react";

import { contactMethodErrorOf, contactSetErrorOf, recentMailQuery } from "../../../core/contacts";
import type { ContactError } from "../../../core/contacts";
import type {
  BlockDestination,
  Contact,
  ContactMergeResult,
  ContactSeparateResult,
  Id,
} from "../../../core/jmap/types";
import { useApp } from "../../state";

export type ContactOutcome<T = undefined> = { ok: true; value: T } | { ok: false; error: ContactError };

export interface ContactPatch {
  isSaved?: boolean;
  isVip?: boolean;
  isBlocked?: boolean;
  blockDestination?: BlockDestination;
  userName?: string | null;
  notes?: string;
}

export interface ContactsPage {
  list: Contact[];
  total: number;
  position: number;
}

export interface RecentMail {
  /** Newest first, at most RECENT_MAIL. */
  ids: Id[];
  /** How many messages the search matched in all. */
  total: number;
  /** The search the list ran, for "See all mail with …". */
  query: string;
  /** Addresses the search syntax cannot carry, so the list does not cover. */
  skipped: string[];
}

/** How many recent messages the page shows. */
export const RECENT_MAIL = 10;

export interface ContactsApi {
  query(args: {
    filter: Record<string, unknown>;
    sort: { property: string; isAscending: boolean }[];
    position: number;
    limit: number;
  }): Promise<ContactOutcome<ContactsPage>>;
  /** Null: the server has no such contact (merged away, or never). */
  get(id: Id): Promise<ContactOutcome<Contact | null>>;
  /** Any contact with exactly this address, saved or not. Null: none. */
  byAddress(address: string): Promise<ContactOutcome<Contact | null>>;
  update(id: Id, patch: ContactPatch): Promise<ContactOutcome>;
  merge(into: Id, from: Id): Promise<ContactOutcome<ContactMergeResult>>;
  separate(id: Id, addressIds: Id[]): Promise<ContactOutcome<ContactSeparateResult>>;
  /** The last messages from or to any of the addresses, through the search
   * path every `from:`/`to:` search takes. The messages land in the mail
   * store, which is where the page reads their subject and date. */
  recentMail(addresses: Contact["addresses"]): Promise<ContactOutcome<RecentMail>>;
}

export function useContactsApi(): ContactsApi {
  const { client, sync } = useApp();
  return useMemo<ContactsApi>(() => {
    const api: ContactsApi = {
      query: async (args) => {
        try {
          const r = await client.contactsQuery(args);
          return { ok: true, value: { list: r.list, total: r.total, position: r.position } };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      get: async (id) => {
        try {
          return { ok: true, value: await client.contactGetOne(id) };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      byAddress: async (address) => {
        try {
          const r = await client.contactsQuery({ filter: { address: address.trim() }, limit: 1 });
          return { ok: true, value: r.list[0] ?? null };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      update: async (id, patch) => {
        try {
          const res = await client.contactSet({ accountId: client.accountId, update: { [id]: { ...patch } } });
          const refused = res.notUpdated?.[id];
          if (refused) return { ok: false, error: contactSetErrorOf(refused) };
          if (res.updated && id in res.updated) return { ok: true, value: undefined };
          return { ok: false, error: "failed" };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      merge: async (into, from) => {
        try {
          return { ok: true, value: await client.contactMerge(into, from) };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      separate: async (id, addressIds) => {
        try {
          return { ok: true, value: await client.contactSeparate(id, addressIds) };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
      recentMail: async (addresses) => {
        const { query, skipped } = recentMailQuery(addresses);
        if (query === "") return { ok: true, value: { ids: [], total: 0, query, skipped } };
        try {
          const { ids, total } = await sync.allMatchingIds({ kind: "search", query }, RECENT_MAIL);
          const shown = ids.slice(0, RECENT_MAIL);
          await sync.loadEmails(shown);
          return { ok: true, value: { ids: shown, total, query, skipped } };
        } catch (err) {
          return { ok: false, error: contactMethodErrorOf(err) };
        }
      },
    };
    return api;
  }, [client, sync]);
}
