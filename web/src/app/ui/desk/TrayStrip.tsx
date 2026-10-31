// SPDX-License-Identifier: AGPL-3.0-only

// THE IN-TRAY STRIP (#375 increment 3).
//
//   In-tray · 12 · oldest waiting 9 d · 3 over a week            [▸] [Open tray]
//
// The tray is everything the user said they would come back to. Its job on the
// desk is to NAG, quietly and permanently: the strip is closed by default and
// states the shape of the backlog in one line rather than spending a screenful
// on it, because the desk is for the mail that arrived today and the tray is
// for the decisions already made about older mail.
//
// OPEN, IT SHOWS AT MOST FIVE ROWS. Not a page, not a scroll region — five,
// newest first, with a link to the real tray for the rest. A disclosure that
// expands into a second full list would make the desk two lists competing for
// the same screen, and the whole point of the strip is that it is a summary you
// can glance past.
//
// WHEN THE WINDOW IS A BOUND, IT SAYS SO. The tray loads a generous page
// (sync.ts TRAY_PAGE = 200) and the counts here are computed over what actually
// loaded, so with more than that the sentence becomes "oldest waiting 9 d among
// the 200 loaded" — a statement about the sample rather than one about the
// tray, which is the difference between a bound and a lie (CLAUDE.md).

import type { ReactNode } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";
import { asOfText, overWeekText, waitText } from "./format";

/** How many rows an opened strip shows. Five is "a glance", not "a list". */
export const TRAY_PREVIEW_ROWS = 5;

export function TrayStrip({
  open,
  onToggle,
  onOpenTray,
  count,
  total,
  oldestWaitMs,
  overWeek,
  asOfSeconds,
  phone = false,
  children,
}: {
  open: boolean;
  onToggle: () => void;
  onOpenTray: () => void;
  /** How many tray rows are actually loaded. */
  count: number;
  /** What the server says the tray holds. */
  total: number;
  /** core/desk.ts trayStats — null for an empty tray, which is not zero. */
  oldestWaitMs: number | null;
  overWeek: number;
  /**
   * How old these counts are, in seconds — supplied ONLY while offline, and
   * `undefined` otherwise (#375 increment 6).
   *
   * The strip states a backlog, and offline that backlog is a snapshot: the
   * numbers are true as of the last sync and are not being refreshed. The
   * desk's own header carries the full sentence; this line has room for the
   * short form, and it belongs on THIS line because these are the numbers it
   * qualifies.
   */
  asOfSeconds?: number | null | undefined;
  /**
   * The phone form (#375 increment 7): ONE TAPPABLE LINE that navigates to the
   * tray, with no disclosure and no preview rows.
   *
   * Not a styling switch — a different control. Five preview rows is a quarter
   * of a phone screen spent on a summary of a list that is one tap away, and a
   * disclosure whose open state is remembered per device would mean arriving at
   * the desk to find the tray already eating the fold. So the whole line means
   * "go to the tray": one tap, one destination, and the `›` says so.
   */
  phone?: boolean;
  /** The preview rows, already limited and rendered by the route. */
  children?: ReactNode;
}): JSX.Element {
  // An empty tray is ONE LINE and no strip body: there is nothing to disclose,
  // and a disclosure button that opens onto nothing is furniture.
  if (total === 0 && count === 0) {
    return <p className="desk-tray is-empty">{t("desk.tray.empty")}</p>;
  }

  const bounded = count < total;
  const parts: string[] = [t("desk.tray.title"), String(total)];
  if (oldestWaitMs !== null) {
    const wait = waitText(oldestWaitMs);
    parts.push(
      bounded ? t("desk.tray.oldestBounded", { wait, loaded: count }) : t("desk.tray.oldest", { wait }),
    );
  }
  if (overWeek > 0) parts.push(overWeekText(overWeek));
  if (asOfSeconds !== undefined) parts.push(asOfText(asOfSeconds));

  if (phone) {
    return (
      <button type="button" className="desk-tray is-link" onClick={onOpenTray}>
        <span className="desk-tray-text">{parts.join(" · ")}</span>
        {/* Decorative: the button's own text is its accessible name, and "go
            to the tray" is what a whole row that is a button already means. */}
        <span className="desk-tray-chevron" aria-hidden="true">
          ›
        </span>
      </button>
    );
  }

  return (
    <div className={open ? "desk-tray is-open" : "desk-tray"}>
      <div className="desk-tray-line">
        <button
          type="button"
          className="desk-tray-toggle"
          aria-expanded={open}
          onClick={onToggle}
        >
          <span className="desk-tray-caret" aria-hidden="true">
            {open ? "▾" : "▸"}
          </span>
          {parts.join(" · ")}
        </button>
        <button type="button" className="link-button" onClick={onOpenTray}>
          {t("desk.tray.open")}
        </button>
      </div>
      {open ? <div className="desk-tray-rows">{children}</div> : null}
    </div>
  );
}
