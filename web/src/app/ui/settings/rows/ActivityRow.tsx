// SPDX-License-Identifier: AGPL-3.0-only

// ACTIVITY (ADR-053): one merged, newest-first list of what happened to your mail
// and access: what AI tools did or were refused, decryptions, permanent deletes,
// sign-ins and sign-outs, and bulk rule and placement actions.
//
// The row has the Devices row's states, for the same reason: the capability may
// be absent, and a server that cannot be asked has not said nothing happened.
//
//   capability absent → "not available on this server"
//   still asking      → no facts
//   asked             → the list, its filters, "Load more"
//
// THE LIST NEVER CLAIMS A TOTAL. There is none: five records with five
// retentions. The footer says "showing the N most recent" and, when the server
// says there is more, offers Load more; at the end it says that is everything
// the records still hold. A source that keeps less (sign-ins: a session is
// swept a week after it ends) says how far back it goes.
//
// Nothing here is message content. A message is linked, never named.

import { useCallback, useEffect, useRef, useState } from "react";
import type { JSX } from "react";

import type { ActivityEntry, ActivityPage, ActivitySource } from "../../../../core/jmap/types";
import { formatRoute } from "../../../../core/routes";
import { keyDateLong } from "../../../../core/userKeys";
import { t, tCount } from "../../../i18n";
import { clockText } from "../../desk/format";
import { Untrusted } from "../apikeys/Untrusted";
import type { StateFact } from "../SettingsRow";
import type { ActivityApi, ActivityQuery } from "../activity/api";
import {
  ACTIVITY_SOURCES,
  coverNote,
  dateRange,
  entryLink,
  entryWords,
  rangeIsBackwards,
  sourceLabel,
} from "../activity/model";

/** The page size the row asks for. The server's bound is 100. */
export const ACTIVITY_PAGE = 50;

export type ActivityReading =
  | { kind: "unavailable" }
  | { kind: "asking" }
  | { kind: "failed" }
  | { kind: "loaded"; entries: ActivityEntry[]; next: string | null; covers: ActivityPage["covers"]; more: "idle" | "loading" | "failed" };

export interface ActivityFilters {
  sources: readonly ActivitySource[];
  from: string;
  to: string;
}

export const NO_FILTERS: ActivityFilters = { sources: ACTIVITY_SOURCES, from: "", to: "" };

function queryOf(f: ActivityFilters, before?: string): ActivityQuery {
  const q: ActivityQuery = { limit: ACTIVITY_PAGE, ...dateRange(f.from, f.to) };
  // Every source selected is no filter at all, so a new source on the server
  // appears without this client having been updated.
  if (f.sources.length < ACTIVITY_SOURCES.length) q.sources = [...f.sources];
  if (before) q.before = before;
  return q;
}

/** Reads the first page once the session has landed (KeysRow's gate) and again
 * whenever a filter changes; `loadMore` appends the next page. */
export function useActivity(
  api: ActivityApi,
  sessionReady: boolean,
  filters: ActivityFilters,
): { reading: ActivityReading; loadMore: () => void } {
  const [reading, setReading] = useState<ActivityReading>({ kind: "asking" });
  const latest = useRef(0);
  const sourcesKey = filters.sources.join(",");
  const blocked = filters.sources.length === 0 || rangeIsBackwards(filters.from, filters.to);

  useEffect(() => {
    if (!sessionReady) return;
    if (api.capability() === null) {
      setReading({ kind: "unavailable" });
      return;
    }
    if (blocked) return;
    const mine = ++latest.current;
    setReading({ kind: "asking" });
    void api.list(queryOf(filters)).then((page) => {
      if (mine !== latest.current) return; // a newer filter is already in flight
      setReading(
        page === null
          ? { kind: "failed" }
          : { kind: "loaded", entries: page.list, next: page.next, covers: page.covers, more: "idle" },
      );
    });
    // `filters` is read through its parts: the object is rebuilt on each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, sessionReady, sourcesKey, filters.from, filters.to, blocked]);

  const readingRef = useRef(reading);
  readingRef.current = reading;
  const loadMore = useCallback(() => {
    const prev = readingRef.current;
    if (prev.kind !== "loaded" || prev.next === null || prev.more === "loading") return;
    const mine = ++latest.current;
    setReading({ ...prev, more: "loading" });
    void api.list(queryOf(filters, prev.next)).then((page) => {
      if (mine !== latest.current) return;
      setReading((cur) => {
        if (cur.kind !== "loaded") return cur;
        if (page === null) return { ...cur, more: "failed" };
        // An id already shown is not shown twice, whatever the server sends.
        const have = new Set(cur.entries.map((e) => e.id));
        return {
          kind: "loaded",
          entries: [...cur.entries, ...page.list.filter((e) => !have.has(e.id))],
          next: page.next,
          covers: page.covers,
          more: "idle",
        };
      });
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, sourcesKey, filters.from, filters.to]);

  return { reading, loadMore };
}

/** The state line. */
export function activityFacts(reading: ActivityReading): StateFact[] {
  if (reading.kind === "unavailable") return [{ text: t("activity.unavailable") }];
  return [];
}

function when(iso: string): string {
  return `${keyDateLong(iso)} ${clockText(iso)}`;
}

function Sentence({ words }: { words: ReturnType<typeof entryWords> }): JSX.Element {
  const at = words.template.indexOf("{name}");
  if (at < 0 || words.name === null) return <>{words.template}</>;
  return (
    <>
      {words.template.slice(0, at)}
      <Untrusted text={words.name} className="activity-name" />
      {words.template.slice(at + "{name}".length)}
    </>
  );
}

export function ActivityBody({
  reading,
  loadMore,
  filters,
  setFilters,
}: {
  reading: ActivityReading;
  loadMore: () => void;
  filters: ActivityFilters;
  setFilters: (f: ActivityFilters) => void;
}): JSX.Element {
  if (reading.kind === "unavailable") return <p className="field-hint">{t("activity.unavailableLong")}</p>;

  const toggle = (s: ActivitySource): void => {
    const on = filters.sources.includes(s);
    setFilters({
      ...filters,
      sources: ACTIVITY_SOURCES.filter((x) => (x === s ? !on : filters.sources.includes(x))),
    });
  };
  const backwards = rangeIsBackwards(filters.from, filters.to);

  return (
    <div className="activity-page">
      <form className="activity-filters" onSubmit={(e) => e.preventDefault()}>
        <fieldset className="activity-sources">
          <legend>{t("activity.filter.show")}</legend>
          {ACTIVITY_SOURCES.map((s) => (
            <label key={s} className="activity-source-choice">
              <input type="checkbox" checked={filters.sources.includes(s)} onChange={() => toggle(s)} /> {sourceLabel(s)}
            </label>
          ))}
        </fieldset>
        <div className="activity-dates">
          <label>
            {t("activity.filter.from")}{" "}
            <input type="date" value={filters.from} onChange={(e) => setFilters({ ...filters, from: e.target.value })} />
          </label>{" "}
          <label>
            {t("activity.filter.to")}{" "}
            <input type="date" value={filters.to} onChange={(e) => setFilters({ ...filters, to: e.target.value })} />
          </label>{" "}
          {filters.from !== "" || filters.to !== "" ? (
            <button type="button" className="button" onClick={() => setFilters({ ...filters, from: "", to: "" })}>
              {t("activity.filter.clearDates")}
            </button>
          ) : null}
        </div>
      </form>

      <div aria-live="polite">
        {filters.sources.length === 0 ? (
          <p className="field-hint activity-none">{t("activity.pickOne")}</p>
        ) : backwards ? (
          <p className="field-hint activity-none">{t("activity.backwards")}</p>
        ) : reading.kind === "asking" ? (
          <p className="field-hint">{t("activity.asking")}</p>
        ) : reading.kind === "failed" ? (
          <p className="field-hint">{t("activity.failed")}</p>
        ) : (
          <Loaded reading={reading} filters={filters} loadMore={loadMore} />
        )}
      </div>
    </div>
  );
}

function Loaded({
  reading,
  filters,
  loadMore,
}: {
  reading: Extract<ActivityReading, { kind: "loaded" }>;
  filters: ActivityFilters;
  loadMore: () => void;
}): JSX.Element {
  const filtered = filters.sources.length < ACTIVITY_SOURCES.length || filters.from !== "" || filters.to !== "";
  if (reading.entries.length === 0) {
    return <p className="field-hint activity-none">{t(filtered ? "activity.none.filtered" : "activity.none")}</p>;
  }
  const note = coverNote(reading, filters.sources);
  return (
    <>
      <ol className="apikey-activity activity-list">
        {reading.entries.map((e) => {
          const words = entryWords(e);
          const link = entryLink(e);
          return (
            <li key={e.id} className={`apikey-event activity-entry${words.tone === "warn" ? " is-warn" : ""}`}>
              <span className="apikey-event-time">{when(e.at)}</span>{" "}
              <span className="activity-tag">{sourceLabel(e.source)}</span>{" "}
              <span className="apikey-event-what">
                <Sentence words={words} />
              </span>
              {link ? (
                <>
                  {" "}
                  <a className="activity-link" href={formatRoute(link.route)}>
                    {link.label}
                  </a>
                </>
              ) : null}
            </li>
          );
        })}
      </ol>
      <p className="field-hint activity-bound">
        {reading.next !== null
          ? tCount("activity.bound.more", reading.entries.length)
          : tCount("activity.bound.end", reading.entries.length)}
      </p>
      {note ? <p className="field-hint activity-cover">{note}</p> : null}
      {reading.more === "failed" ? <p className="field-hint">{t("activity.moreFailed")}</p> : null}
      {reading.next !== null ? (
        <button type="button" className="button" disabled={reading.more === "loading"} onClick={loadMore}>
          {t(reading.more === "loading" ? "activity.loading" : "activity.loadMore")}
        </button>
      ) : null}
    </>
  );
}
