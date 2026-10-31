// SPDX-License-Identifier: AGPL-3.0-only

// THE CONTACTS LIST (#816): the people you correspond with, not every address
// that ever appeared in your mail. Only SAVED contacts are listed (decided
// 2026-10-22): sending to someone saves them, "Add to contacts" saves anyone
// else, and VIP saves. Blocked senders are their own filter, because a blocked
// sender is usually someone you never saved; the VIP filter likewise asks for
// every VIP, because un-saving a VIP keeps the marker (core/contacts.ts).
//
// THE FILTER, SORT AND SEARCH TEXT LIVE IN THE URL (core/routes.ts), and this
// component stays mounted while a contact is open — beside it on a wide
// screen, hidden behind it on a phone — so going back returns to the same
// rows, the same "Show more" pages and the same scroll position.
//
// A PAGE SAYS ITS BOUND: "Showing 50 of 53", never 50 rows presented as all of
// them (CLAUDE.md, #155). `total` is the server's.

import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { JSX } from "react";

import { CONTACTS_FILTERS, CONTACTS_PAGE, contactIdentity, contactLabel, contactsQueryArgs, hasSavedSurface } from "../../../core/contacts";
import type { ContactError, ContactsFilter } from "../../../core/contacts";
import type { Contact } from "../../../core/jmap/types";
import { contactsListOf, formatRoute } from "../../../core/routes";
import type { ContactsRoute } from "../../../core/routes";
import { keyDateLong } from "../../../core/userKeys";
import { looksLikeAddress } from "../compose/chips";
import { loadPosition, navigate, savePosition } from "../../routing";
import { t, tCount } from "../../i18n";
import type { MessageKey } from "../../i18n";
import type { ContactsApi } from "./api";
import { contactErrorText } from "./text";

const FILTER_LABEL: Record<ContactsFilter, MessageKey> = {
  saved: "contacts.filter.saved",
  vip: "contacts.filter.vip",
  key: "contacts.filter.key",
  blocked: "contacts.filter.blocked",
};

const EMPTY_TEXT: Record<ContactsFilter, MessageKey> = {
  saved: "contacts.empty.saved",
  vip: "contacts.empty.vip",
  key: "contacts.empty.key",
  blocked: "contacts.empty.blocked",
};

type Reading =
  | { kind: "asking" }
  | { kind: "failed"; error: ContactError }
  | { kind: "unavailable" }
  | { kind: "loaded"; list: Contact[]; total: number };

/** Where the list's scroll is remembered (routing.ts savePosition). */
function positionKey(r: ContactsRoute): string {
  return `contacts:${r.filter ?? "saved"}:${r.sort ?? "name"}:${r.q ?? ""}`;
}

/** A plain click navigates through the router (so Back and Escape agree); a
 * modified one is left to the browser (a new tab, a copied link). */
function plainClick(ev: React.MouseEvent): boolean {
  return ev.button === 0 && !ev.metaKey && !ev.ctrlKey && !ev.shiftKey && !ev.altKey;
}

export function ContactsList({
  api,
  route,
  version,
  onChanged,
  onMessage,
}: {
  api: ContactsApi;
  route: ContactsRoute;
  /** Bumped by the page after a change, so the rows are read again. */
  version: number;
  onChanged: () => void;
  onMessage: (message: string) => void;
}): JSX.Element {
  const filter = route.filter ?? "saved";
  const sort = route.sort ?? "name";
  const text = route.q ?? "";
  const listRoute = contactsListOf(route);
  const [reading, setReading] = useState<Reading>({ kind: "asking" });
  const [more, setMore] = useState(false);
  const [box, setBox] = useState(text);
  const [adding, setAdding] = useState(false);
  const [addAddress, setAddAddress] = useState("");
  const [addBusy, setAddBusy] = useState(false);
  const [addError, setAddError] = useState<string | null>(null);
  const searchId = useId();
  const addId = useId();
  const headingId = useId();
  const paneRef = useRef<HTMLElement | null>(null);
  const restore = useRef<number | null>(loadPosition(positionKey(route))?.scrollTop ?? null);
  const shown = reading.kind === "loaded" ? reading.list.length : 0;
  const shownRef = useRef(0);
  shownRef.current = shown;

  // The box follows the URL (Back to a list with other text), and the URL
  // follows the box after a pause — replacing the entry, so a typed word is
  // not ten steps of history.
  useEffect(() => setBox(text), [text]);
  useEffect(() => {
    if (box === text) return;
    const timer = setTimeout(() => {
      const q = box.trim() === "" ? {} : { q: box };
      const { q: _drop, ...rest } = route;
      navigate({ ...rest, ...q }, { replace: true });
    }, 250);
    return () => clearTimeout(timer);
    // `route` changes with every navigation; the pause is about the box.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [box]);

  const read = useCallback(
    async (limit: number): Promise<Reading> => {
      const out = await api.query(contactsQueryArgs({ filter, sort, text, position: 0, limit }));
      if (!out.ok) return out.error === "unavailable" ? { kind: "unavailable" } : { kind: "failed", error: out.error };
      // A server before #816 ignores `isSaved` and answers with contacts that
      // carry no saved state at all: that is "cannot say", not a list.
      if (out.value.list.some((c) => !hasSavedSurface(c))) return { kind: "unavailable" };
      return { kind: "loaded", list: out.value.list, total: out.value.total };
    },
    [api, filter, sort, text],
  );

  // A new filter, sort or text starts from the first page. A change made on a
  // contact's page (version) re-reads as many rows as were showing, so the
  // list does not shrink under the reader.
  const lastQuery = useRef("");
  useEffect(() => {
    let cancelled = false;
    const queryKey = `${filter}|${sort}|${text}`;
    const sameQuery = lastQuery.current === queryKey;
    lastQuery.current = queryKey;
    const limit = sameQuery ? Math.min(100, Math.max(CONTACTS_PAGE, shownRef.current)) : CONTACTS_PAGE;
    if (!sameQuery) setReading({ kind: "asking" });
    void read(limit).then((r) => {
      if (!cancelled) setReading(r);
    });
    return () => {
      cancelled = true;
    };
  }, [read, filter, sort, text, version]);

  const showMore = async (): Promise<void> => {
    if (reading.kind !== "loaded") return;
    setMore(true);
    const out = await api.query(contactsQueryArgs({ filter, sort, text, position: reading.list.length }));
    setMore(false);
    if (!out.ok) {
      onMessage(contactErrorText(out.error));
      return;
    }
    const seen = new Set(reading.list.map((c) => c.id));
    setReading({
      kind: "loaded",
      list: [...reading.list, ...out.value.list.filter((c) => !seen.has(c.id))],
      total: out.value.total,
    });
  };

  // Back from a contact: put the list where it was. On a phone the list was
  // hidden behind the page, which is when a scroll position is lost.
  useLayoutEffect(() => {
    if (route.id || restore.current === null || reading.kind !== "loaded") return;
    if (paneRef.current) paneRef.current.scrollTop = restore.current;
    restore.current = null;
  }, [route.id, reading]);

  const open = (c: Contact): void => {
    const top = paneRef.current?.scrollTop ?? 0;
    restore.current = top;
    savePosition(positionKey(route), { cursor: c.id, scrollTop: top });
    navigate({ ...listRoute, id: c.id });
  };

  const setFilter = (f: ContactsFilter): void => {
    const { filter: _f, ...rest } = route;
    navigate(f === "saved" ? rest : { ...rest, filter: f }, { replace: true });
  };

  const setSort = (s: "name" | "last"): void => {
    const { sort: _s, ...rest } = route;
    navigate(s === "name" ? rest : { ...rest, sort: s }, { replace: true });
  };

  const add = async (): Promise<void> => {
    const address = addAddress.trim();
    if (!looksLikeAddress(address)) return;
    setAddBusy(true);
    setAddError(null);
    const found = await api.byAddress(address);
    if (!found.ok) {
      setAddBusy(false);
      setAddError(contactErrorText(found.error));
      return;
    }
    const c = found.value;
    if (c === null) {
      setAddBusy(false);
      setAddError(t("contacts.add.unknown", { address }));
      return;
    }
    if (c.isSaved !== true) {
      const saved = await api.update(c.id, { isSaved: true });
      if (!saved.ok) {
        setAddBusy(false);
        setAddError(contactErrorText(saved.error));
        return;
      }
      onMessage(t("contacts.added", { name: contactLabel(c) }));
      onChanged();
    }
    setAddBusy(false);
    setAddAddress("");
    setAdding(false);
    navigate({ ...listRoute, id: c.id });
  };

  return (
    <section className="list-pane contacts-list-pane" aria-labelledby={headingId} ref={paneRef}>
      <header className="contacts-list-header">
        <h1 id={headingId}>{t("contacts.title")}</h1>
        <label className="field-label" htmlFor={searchId}>
          {t("contacts.search")}
        </label>
        <input
          id={searchId}
          className="text-input contacts-search"
          type="search"
          autoComplete="off"
          value={box}
          onChange={(ev) => setBox(ev.target.value)}
        />
        <div className="contacts-filters" role="group" aria-label={t("contacts.filter.label")}>
          {CONTACTS_FILTERS.map((f) => (
            <button
              key={f}
              type="button"
              className={`chip-button${f === filter ? " is-on" : ""}`}
              aria-pressed={f === filter}
              onClick={() => setFilter(f)}
            >
              {t(FILTER_LABEL[f])}
            </button>
          ))}
        </div>
        <label className="contacts-sort">
          <span className="field-label">{t("contacts.sort")}</span>
          <select
            className="select"
            value={sort}
            onChange={(ev) => setSort(ev.target.value === "last" ? "last" : "name")}
          >
            <option value="name">{t("contacts.sort.name")}</option>
            <option value="last">{t("contacts.sort.last")}</option>
          </select>
        </label>
      </header>

      {reading.kind === "asking" ? (
        <p className="field-hint contacts-state" role="status">
          {t("contacts.loading")}
        </p>
      ) : reading.kind === "unavailable" ? (
        <p className="field-hint contacts-state">{t("contacts.unavailable")}</p>
      ) : reading.kind === "failed" ? (
        <p className="field-error contacts-state" role="alert">
          {contactErrorText(reading.error)}
        </p>
      ) : reading.list.length === 0 ? (
        <p className="field-hint contacts-state contacts-empty">
          {text.trim() !== "" ? t("contacts.empty.search", { text: text.trim() }) : t(EMPTY_TEXT[filter])}
        </p>
      ) : (
        <>
          <ul className="contacts-rows" aria-label={t(FILTER_LABEL[filter])}>
            {reading.list.map((c) => {
              // The one formatter (core/contacts.ts): cleaned text, and the
              // address shown whenever it is not the name itself.
              const { name, address } = contactIdentity(c);
              const to = { ...listRoute, id: c.id };
              return (
                <li key={c.id}>
                  <a
                    className="contacts-row"
                    href={formatRoute(to)}
                    aria-current={route.id === c.id ? "page" : undefined}
                    onClick={(ev) => {
                      if (!plainClick(ev)) return;
                      ev.preventDefault();
                      open(c);
                    }}
                  >
                    <span className="contacts-row-name">{name}</span>
                    {address !== name ? <span className="contacts-row-address">{address}</span> : null}
                    <span className="contacts-row-meta">
                      {c.lastMailAt ? (
                        <span className="contacts-row-date">
                          {t("contacts.row.last", { date: keyDateLong(c.lastMailAt) })}
                        </span>
                      ) : null}
                      {c.isVip ? <span className="contacts-chip">{t("contacts.chip.vip")}</span> : null}
                      {c.keys && c.keys.length > 0 ? (
                        <span className="contacts-chip">{t("contacts.chip.key")}</span>
                      ) : null}
                      {c.isBlocked ? (
                        <span className="contacts-chip is-warn">{t("contacts.chip.blocked")}</span>
                      ) : null}
                    </span>
                  </a>
                </li>
              );
            })}
          </ul>
          <p className="field-hint contacts-bound">
            {reading.list.length >= reading.total
              ? tCount("contacts.bound.all", reading.total)
              : t("contacts.bound.some", { shown: reading.list.length, total: reading.total })}
          </p>
          {reading.list.length < reading.total ? (
            <button type="button" className="button contacts-more" disabled={more} onClick={() => void showMore()}>
              {more ? t("contacts.more.busy") : t("contacts.more")}
            </button>
          ) : null}
        </>
      )}

      <div className="contacts-add">
        <button
          type="button"
          className="button button-quiet"
          aria-expanded={adding}
          onClick={() => {
            setAdding((v) => !v);
            setAddError(null);
          }}
        >
          {t("contacts.add.open")}
        </button>
        {adding ? (
          <form
            className="contacts-add-form"
            onSubmit={(ev) => {
              ev.preventDefault();
              void add();
            }}
          >
            <label className="field-label" htmlFor={addId}>
              {t("contacts.add.label")}
            </label>
            <div className="contacts-add-row">
              <input
                id={addId}
                className="text-input"
                type="email"
                autoComplete="off"
                value={addAddress}
                onChange={(ev) => setAddAddress(ev.target.value)}
              />
              <button
                type="submit"
                className="button"
                disabled={addBusy || !looksLikeAddress(addAddress.trim())}
              >
                {addBusy ? t("contacts.add.busy") : t("contacts.add.submit")}
              </button>
            </div>
            {addError ? (
              <p className="field-error" role="alert">
                {addError}
              </p>
            ) : null}
          </form>
        ) : null}
      </div>
    </section>
  );
}
