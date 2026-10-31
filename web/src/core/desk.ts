// SPDX-License-Identifier: AGPL-3.0-only

// The DESK's arithmetic (#375): which sections it has, what each one asks the
// server for, and every number its chrome states out loud. DOM-free and
// framework-agnostic (spec §12) — no React, no CSS, nothing that knows a pixel
// except by being handed one.
//
// ---------------------------------------------------------------------------
// What the desk is
// ---------------------------------------------------------------------------
// The inbox is every unactioned message. The desk arranges it: each message is
// in exactly one AREA (or in none — "Needs sorting") and on exactly one side of
// the LINE (DeskPreferences.lineThreshold, applied to the triage score).
//
//   Review first    every area's rows that are ON THE SURFACE — scored at or
//                   above the line, OR NOT SCORED AT ALL, because unjudged mail
//                   is never hidden — plus Needs sorting, which contributes ALL
//                   of its mail whatever its score, since nothing has claimed it
//                   and no drawer will ever sweep it.
//   Everything else one DRAWER per area: what fell below the line, closed by
//                   default, opened on demand.
//   The tray        flagged mail, across areas and across both tabs.
//
// Everything here is a PURE FUNCTION of what the server said. That is the point:
// the counts in the header sentence, the filters the sections query with, and
// the buckets the tray groups by are each one testable expression rather than
// arithmetic scattered through JSX — and the desk's whole claim is that its
// numbers are true.

import type { Area, AreaSummaryRow, DeskPreferences, Email, EmailFilter, Id } from "./jmap/types";
import { KEYWORD_FLAGGED } from "./jmap/types";

/** The desk's two tabs. Matches routes.ts InboxTab, which is what a URL
 * carries — the same two names on both sides, so a route never has to be
 * translated into a data-layer concept. */
export type DeskTab = "review" | "rest";

/** The three kinds of list the desk renders. */
export type SectionKind = "tray" | "surface" | "drawer";

/** One section of the desk: a kind, and the area it belongs to.
 *
 * `areaId: null` with `kind: "surface"` is NEEDS SORTING — mail no area claims.
 * `areaId` is ignored for the tray, which spans every area by definition. */
export interface DeskSection {
  kind: SectionKind;
  areaId: Id | null;
}

/**
 * The store key one section's rows live under (`MailState.sections`).
 *
 * SCOPE-QUALIFIED, and that is not decoration: the account switcher changes
 * which mail an area holds, so "Bills, surface" means two different lists under
 * two different accounts. Sharing a key between them would show the previous
 * account's rows for as long as the new query took to come back — mail
 * attributed to an account it is not in.
 *
 * The scope leads so that every key for one scope shares a prefix, which is what
 * makes "forget the other account's sections" a single predicate
 * (sectionKeyPrefix). `all` cannot collide with a real id: scopes are ULIDs,
 * which are uppercase Crockford base32.
 */
export function sectionKey(kind: SectionKind, areaId: Id | null, scope: Id | null): string {
  return `${scope ?? "all"}/${kind}/${areaId ?? "none"}`;
}

/** The prefix every `sectionKey` for this scope starts with — so a caller can
 * drop the sections belonging to any OTHER scope without having to enumerate
 * them (an open drawer's key is not in any list the desk is currently drawing). */
export function sectionKeyPrefix(scope: Id | null): string {
  return `${scope ?? "all"}/`;
}

/**
 * The sections a tab is made of, in the order they are drawn.
 *
 * Review first: every area, by `position` ascending (ties broken by id, so the
 * order is total and does not depend on the order the server happened to list
 * them in), then NEEDS SORTING LAST. Last because it is the leftovers: an area
 * is somewhere the user decided mail should go, and unsorted mail is the pile
 * that decision has not been made about yet.
 *
 * Everything else: one drawer per area, same order. There is NO Needs-sorting
 * drawer — nothing sweeps unsorted mail (the drawer sweep resolves a message
 * through its area, and unsorted mail has none), so a drawer there would be a
 * container that never fills and never empties.
 *
 * A HIDDEN AREA STILL HAS ITS SECTIONS. `hiddenFromRail` hides the navigation
 * entry, not the mail: dropping the section would silently disappear every
 * message the area holds, which is the one thing a mail client must never do.
 */
export function deskSections(tab: DeskTab, areas: Area[]): DeskSection[] {
  const ordered = [...areas].sort((a, b) =>
    a.position !== b.position ? a.position - b.position : a.id.localeCompare(b.id),
  );
  const kind: SectionKind = tab === "review" ? "surface" : "drawer";
  const out: DeskSection[] = ordered.map((a) => ({ kind, areaId: a.id }));
  if (tab === "review") out.push({ kind: "surface", areaId: null });
  return out;
}

/**
 * The Email/query filter one section asks the server for.
 *
 * Every desk section is `onDesk: true` — live in INBOX or a folder that feeds
 * the desk, and not junk, trashed, vanished or a draft. That is the EXACT
 * predicate Area/getSummary counts over, which is what makes a section's rows
 * and the count printed beside them incapable of disagreeing — WITH ONE
 * EXCEPTION: the surface/drawer and area rows below also add `isSnoozed:
 * false`, but `AreaSummary` (internal/store/areas_resolve.go) has no snooze
 * predicate at all, so a snoozed message is excluded from a section's rows
 * while still counted in the number printed beside it.
 *
 * NEEDS SORTING CARRIES NO `onSurface`, deliberately. Its mail is on the surface
 * whatever its score: nothing has claimed it, so no drawer will ever sweep it,
 * and splitting it at the line would leave the low-scoring half in a drawer that
 * does not exist — mail with nowhere to be shown.
 *
 * The TRAY is `hasKeyword: $flagged` and nothing else. No `onDesk` (a message
 * you flagged and then filed is still something you said you would come back
 * to), and no snooze condition — the same choice the Starred view makes: a
 * question the user asked outright is not answered with a silent omission. The
 * server already excludes trash.
 */
export function sectionFilter(section: DeskSection, scope: Id | null): EmailFilter {
  const inAccount: EmailFilter = scope ? { inAccount: scope } : {};
  if (section.kind === "tray") return { hasKeyword: KEYWORD_FLAGGED, ...inAccount };
  const side: EmailFilter = section.kind === "surface" ? { onSurface: true } : { onSurface: false };
  if (section.areaId === null) {
    // Needs sorting. The surface form takes no side of the line at all; the
    // drawer form is unreachable through deskSections and exists only so this
    // function is total — if something ever asks for it, it gets the honest
    // query rather than another area's mail.
    return {
      unsorted: true,
      onDesk: true,
      ...(section.kind === "surface" ? {} : side),
      isSnoozed: false,
      ...inAccount,
    };
  }
  return { inArea: section.areaId, onDesk: true, ...side, isSnoozed: false, ...inAccount };
}

/**
 * The two counts in the header sentence — "6 to review · 37 in drawers".
 *
 * `review` is everything the Review-first tab will show: each area's surface
 * plus its unscored mail (which the surface query returns, because unjudged mail
 * is never hidden), plus ALL THREE counts of the Needs-sorting row, since that
 * section takes its drawer-side mail too.
 *
 * `drawers` is what Everything else holds: the areas' drawers only. The
 * Needs-sorting row contributes NOTHING here — its below-the-line mail is
 * already counted in `review`, and counting it twice would make the sentence
 * claim more mail than the desk has.
 */
export function headerCounts(rows: AreaSummaryRow[]): { review: number; drawers: number } {
  let review = 0;
  let drawers = 0;
  for (const r of rows) {
    if (r.areaId === null) {
      review += r.surface + r.drawer + r.unscored;
      continue;
    }
    review += r.surface + r.unscored;
    drawers += r.drawer;
  }
  return { review, drawers };
}

/**
 * One area's counts, for its rail entry and its section headers.
 *
 * Returns zeroes for an area the summary has no row for. That is the honest
 * answer to "how much mail is in an area the server did not report", and it
 * keeps a rail rendered from `areas` from going blank when a summary is one
 * change behind.
 */
export function areaCounts(
  rows: AreaSummaryRow[],
  areaId: Id | null,
): { review: number; drawer: number } {
  const row = rows.find((r) => r.areaId === areaId);
  if (!row) return { review: 0, drawer: 0 };
  // Needs sorting has no drawer at all (see sectionFilter): all of its mail is
  // on the surface, so reporting a drawer count would name a list the desk
  // never draws.
  if (areaId === null) return { review: row.surface + row.drawer + row.unscored, drawer: 0 };
  return { review: row.surface + row.unscored, drawer: row.drawer };
}

/** A day and a week in milliseconds — the tray's units. */
const DAY_MS = 24 * 60 * 60 * 1000;
const WEEK_MS = 7 * DAY_MS;
const MONTH_MS = 30 * DAY_MS;

/**
 * What the in-tray strip says about itself: how many, how long the oldest has
 * waited, and how many have been waiting a WEEK OR MORE.
 *
 * The wait runs from `flaggedAt` — when the user put the message in the tray —
 * and falls back to `receivedAt` only when the server did not send one (an older
 * server, or a projection that did not ask for it). The fallback is a different
 * number, not the same one: a message flagged out of a month-old thread has
 * waited a day, and ageing it by arrival would nag about a decision the user
 * made this morning.
 *
 * `oldestWaitMs` is NULL for an empty tray rather than 0 — "nothing is waiting"
 * and "something arrived this instant" are different states, and 0 reads as the
 * second. `overWeek` counts everything at or past a week, INCLUDING what is past
 * a month: it is the nag, and mail does not stop being overdue by getting older.
 *
 * A timestamp that does not parse counts as a wait of zero rather than poisoning
 * the maximum with NaN — an unreadable date is not evidence that something has
 * been waiting.
 */
export function trayStats(
  emails: Pick<Email, "flaggedAt" | "receivedAt">[],
  now: number,
): { count: number; oldestWaitMs: number | null; overWeek: number } {
  let oldest: number | null = null;
  let overWeek = 0;
  for (const e of emails) {
    const since = Date.parse(e.flaggedAt ?? e.receivedAt);
    const wait = Number.isNaN(since) ? 0 : now - since;
    if (oldest === null || wait > oldest) oldest = wait;
    if (wait >= WEEK_MS) overWeek += 1;
  }
  return { count: emails.length, oldestWaitMs: oldest, overWeek };
}

/** How long a tray row has been waiting, as the strip groups them. */
export type WaitBucket = "today" | "thisWeek" | "overWeek" | "overMonth";

/**
 * Which group a wait falls in: under a day, under a week, under a month, or
 * older.
 *
 * The strip groups by this rather than printing 40 exact ages, because the
 * question it answers is "what have I been ignoring", and that is a shape, not a
 * list of durations. A negative wait — a clock askew, a `flaggedAt` in the
 * future — reads as `today`: it is certainly not overdue.
 */
export function waitBucket(waitMs: number): WaitBucket {
  if (waitMs < DAY_MS) return "today";
  if (waitMs < WEEK_MS) return "thisWeek";
  if (waitMs < MONTH_MS) return "overWeek";
  return "overMonth";
}

/**
 * The tray, split into the four groups the tray page draws — in the order it
 * draws them, with the empty ones left out.
 *
 * It is `waitBucket` applied per row and nothing else, but it is a FUNCTION
 * rather than a `reduce` in JSX for the reason the rest of this module exists:
 * the groups are a claim about how long the user has been ignoring things, the
 * boundaries between them are exactly the sort of arithmetic that is wrong by
 * one unit for a year, and a test can only pin them if they are reachable at a
 * fixed `now`.
 *
 * THE WAIT IS THE SAME WAIT `trayStats` MEASURES — `flaggedAt`, falling back to
 * `receivedAt`, and an unparseable timestamp counting as zero. It has to be:
 * the page's header sentence says "oldest waiting 9 d" from one of these
 * functions and the heading above that row is drawn by the other, and the two
 * disagreeing by a definition would be a screen arguing with itself.
 *
 * ORDER WITHIN A GROUP IS THE ORDER IT WAS GIVEN. The caller has already sorted
 * (pinned first, then arrival — store.ts sectionIds), and re-sorting by wait
 * here would silently override that with a second opinion.
 */
export function groupByWait<T extends Pick<Email, "flaggedAt" | "receivedAt">>(
  emails: readonly T[],
  now: number,
): { bucket: WaitBucket; emails: T[] }[] {
  const order: WaitBucket[] = ["today", "thisWeek", "overWeek", "overMonth"];
  const groups = new Map<WaitBucket, T[]>();
  for (const e of emails) {
    const since = Date.parse(e.flaggedAt ?? e.receivedAt);
    const wait = Number.isNaN(since) ? 0 : now - since;
    const bucket = waitBucket(wait);
    const list = groups.get(bucket);
    if (list) list.push(e);
    else groups.set(bucket, [e]);
  }
  // Empty groups are OMITTED rather than drawn with a zero: "Over a month · 0"
  // is a heading for a list that does not exist, and four of them above one row
  // would make an empty tray look like a full one.
  return order
    .filter((bucket) => (groups.get(bucket)?.length ?? 0) > 0)
    .map((bucket) => ({ bucket, emails: groups.get(bucket) as T[] }));
}

/** What the RAIL prints beside its entries, or nulls when nothing has been
 * measured yet. Every field is `number | null` on purpose: a badge is blank
 * before the first summary lands, because a `0` that means "not loaded" is a
 * claim about the user's mail that nobody made (CLAUDE.md). */
export interface RailCounts {
  /** Inbox — everything unactioned: the tray, plus what is waiting to be
   * reviewed, plus what the line put in drawers. */
  inbox: number | null;
  /** In-tray — what the tray section's own query counted. */
  tray: number | null;
  /** All mail — the count `loadDesk` asks for alongside the summary. */
  allMail: number | null;
  /** Needs sorting — ALL THREE buckets of the `areaId: null` row, since that
   * section shows all of its mail whatever the score (sectionFilter). */
  needsSorting: number | null;
}

/**
 * The rail's numbers, derived once (#375).
 *
 * INBOX INCLUDES THE TRAY. The rail's Inbox entry means "everything that is
 * still waiting on you", and a flagged message is the most waiting-on-you thing
 * there is — it is mail you told yourself to come back to. Counting only the
 * desk's two halves would make the Inbox badge smaller than the sum of the two
 * entries printed directly beneath it, which is the kind of arithmetic a user
 * checks once and then stops believing.
 *
 * The tray count is a SEPARATE MEASUREMENT from the summary: `Area/getSummary`
 * counts what is on the desk, and the tray is deliberately not restricted to
 * that (a message you flagged and then filed is still in the tray —
 * sectionFilter). So it comes from the tray section's own `total`, and with no
 * tray loaded there is no Inbox number to print either: a sum with a missing
 * term is not a smaller sum, it is an unknown one.
 */
export function railCounts(
  summary: { rows: AreaSummaryRow[]; allMail: number | null } | null,
  /** The tray section's server `total`, or null when the tray has never been
   * queried at this scope. */
  trayTotal: number | null,
): RailCounts {
  if (!summary) return { inbox: null, tray: null, allMail: null, needsSorting: null };
  const { review, drawers } = headerCounts(summary.rows);
  return {
    inbox: trayTotal === null ? null : trayTotal + review + drawers,
    tray: trayTotal,
    allMail: summary.allMail,
    needsSorting: areaCounts(summary.rows, null).review,
  };
}

/**
 * How many days a drawer keeps mail before the sweep archives it, or null when
 * this area's drawer does not sweep — or when nothing here knows the period.
 *
 * The four cases, in order:
 *   1. `autoArchive: false` — off. Null, and "off" is only ever expressed this
 *      way (a null `autoArchiveDays` has never meant off).
 *   2. an explicit `autoArchiveDays` — that, whatever the login default is.
 *   3. null days, preferences loaded — the login default.
 *   4. null days, preferences NOT loaded — null. It is unknown, and a client
 *      that guessed would print a promise about mail from a number it invented.
 *
 * ⚠ THIS IS THE PERIOD, NOT A PROMISE THAT THE SWEEP RUNS. The sweep has a
 * login-level master switch (`DeskPreferences.drawerSweepEnabled`, #443) which
 * DEFAULTS TO FALSE, so on an instance nobody has turned it on every area still
 * reports a period here and none of them sweeps. Anything rendering
 * "auto-archives after N days" must check that switch first — this function
 * deliberately answers the narrower question, because "what is this area's
 * period" and "is the sweep on" are two facts and folding them would lose one.
 */
export function drawerPeriodDays(area: Area, prefs: DeskPreferences | null): number | null {
  if (!area.autoArchive) return null;
  if (area.autoArchiveDays !== null) return area.autoArchiveDays;
  return prefs ? prefs.drawerDefaultDays : null;
}

/**
 * How long ago the last successful round trip was, in whole seconds — the
 * header's "synced 40 s ago" — or null before the first one.
 *
 * Null rather than 0, for the same reason `trayStats.oldestWaitMs` is: "we have
 * never reached the server" must not render as "synced just now". Clamped at
 * zero so a clock that steps backwards says "just now" rather than counting up
 * from a negative.
 */
export function syncedAgoSeconds(lastSyncAt: number | null, now: number): number | null {
  if (lastSyncAt === null) return null;
  return Math.max(0, Math.floor((now - lastSyncAt) / 1000));
}

/** The slice of a long list that is worth putting in the DOM, plus the two
 * measurements that keep the scrollbar honest while the rest is absent. */
export interface VirtualWindow {
  /** First index to render. */
  start: number;
  /** One PAST the last index to render (exclusive, so `ids.slice(start, end)`). */
  end: number;
  /** How far down the spacer above the rendered rows must push them. */
  offsetTop: number;
  /** The height the whole list would have if every row were rendered — what the
   * scrollbar is sized from. */
  totalHeight: number;
}

/**
 * Which rows of a fixed-height list to render for a given scroll position.
 *
 * The desk's areas can each hold thousands of messages, and the drawer sweep
 * means a drawer legitimately holds months of mail; rendering all of it is how a
 * list stops scrolling at 60fps. This is the arithmetic, alone and testable —
 * the component that owns the scroll container supplies the three measurements
 * and renders `ids.slice(start, end)` inside a spacer of `totalHeight`.
 *
 * `overscan` rows are rendered beyond each edge so a fast scroll does not show
 * blank space before React catches up. Degenerate inputs answer with an empty
 * window rather than a guess: no rows, or a row height of zero or less (an
 * element that has not been measured yet), means there is nothing to place.
 * A negative `scrollTop` — rubber-band overscroll — reads as the top.
 */
export function virtualWindow(
  scrollTop: number,
  viewportHeight: number,
  rowHeight: number,
  count: number,
  overscan = 6,
): VirtualWindow {
  if (rowHeight <= 0 || count <= 0) return { start: 0, end: 0, offsetTop: 0, totalHeight: 0 };
  const totalHeight = count * rowHeight;
  const top = Math.max(0, scrollTop);
  const first = Math.floor(top / rowHeight);
  // +1 because the viewport almost never divides evenly into rows: scrolled to
  // any position that is not an exact multiple, one more row is partly visible
  // at the bottom, and leaving it out shows a gap at the fold.
  const visible = Math.ceil(Math.max(0, viewportHeight) / rowHeight) + 1;
  const start = Math.min(Math.max(0, first - overscan), count);
  const end = Math.max(start, Math.min(count, first + visible + overscan));
  return { start, end, offsetTop: start * rowHeight, totalHeight };
}

/** A window plus the one index that had to be kept in the DOM behind its back. */
export interface PinnedWindow extends VirtualWindow {
  /**
   * An index the caller must render IN ADDITION to `[start, end)`, or null when
   * the window already contains it (or there is nothing to pin).
   *
   * There is deliberately no separate offset for it: every row in a fixed-height
   * list is placed at `index * rowHeight`, so the pinned one is placed by exactly
   * the expression the others are. A second placement rule here would be a second
   * chance for the two to disagree by a pixel.
   */
  pinned: number | null;
}

/**
 * The same window, with ONE INDEX GUARANTEED TO BE IN THE DOM (#447).
 *
 * ⚠ THIS EXISTS BECAUSE OF `aria-activedescendant`, not because of scrolling. The
 * desk's list is a grid whose focus lives on the CONTAINER and whose current row
 * is named by id (ui/desk/VirtualList.tsx says why that rather than a roving
 * `tabIndex`: a virtualised row can unmount while it holds focus, and focus would
 * fall to `<body>`). An `aria-activedescendant` pointing at an id that is not in
 * the document is worse than none at all — the grid claims a current row and the
 * screen reader finds nothing there, so it announces nothing and the keyboard
 * looks broken while every keystroke is in fact working.
 *
 * The cursor is normally inside the window, because moving it scrolls it into
 * view. It is OUTSIDE it whenever the two are moved independently: the wheel, a
 * scrollbar drag, a touch flick — the cursor stays where the keyboard left it and
 * the window walks away from it. That is the case this covers.
 *
 * Kept here, beside `virtualWindow` and DOM-free, rather than as a special case
 * inside the component: "which rows are in the DOM" is one piece of arithmetic and
 * it is already tested here.
 */
export function pinnedWindow(
  win: VirtualWindow,
  pin: number,
  count: number,
  rowHeight: number,
): PinnedWindow {
  // ⚠ `rowHeight <= 0` MEANS NOTHING HAS BEEN MEASURED, and that is the one state
  // where pinning is wrong rather than merely unnecessary: the caller lays the list
  // out in ordinary flow until it has a row height, where an absolutely-positioned
  // extra row would be drawn on top of the list instead of at its own offset.
  //
  // An EMPTY window is deliberately NOT excluded. A list that shrank under an open
  // scroll position answers `start === end === count` — no rows at all — and that is
  // exactly when the active descendant most needs to exist.
  const pinnable = rowHeight > 0 && pin >= 0 && pin < count;
  const outside = pin < win.start || pin >= win.end;
  return { ...win, pinned: pinnable && outside ? pin : null };
}
