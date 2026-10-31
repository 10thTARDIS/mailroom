// SPDX-License-Identifier: AGPL-3.0-only

// THE WORDS THE DESK'S CHROME IS MADE OF (#375 increment 3).
//
// Every sentence the desk states about itself — "synced 40 s ago", "oldest
// waiting 9 d", "3 over a week", "Show 50 more of 218", the time on a row — is
// derived here rather than inline in JSX, for the same reason core/desk.ts
// holds the arithmetic: these are claims about the user's mail, and a claim
// worth making is a claim worth testing at a fixed `now`.
//
// Pure, and dependency-free apart from the message catalog: no React, no store,
// no `Date.now()` of its own. Every function that needs the time takes it.

import { syncedAgoSeconds } from "../../../core/desk";
import type { HeaderState } from "../../../core/headerState";
import { t, tCount } from "../../i18n";

const MINUTE_S = 60;
const HOUR_S = 60 * 60;
const DAY_S = 24 * HOUR_S;

const HOUR_MS = 60 * 60 * 1000;
const DAY_MS = 24 * HOUR_MS;

/**
 * How long ago the last successful round trip was, as the header says it.
 *
 * NEVER A BARE CLOCK. "synced at 14:02" makes the reader do the subtraction,
 * and gets it wrong across a suspend/resume or a timezone change; the question
 * behind the sentence is "is what I am looking at current", and the answer to
 * that is a duration.
 *
 * `null` — no successful sync yet — is its own sentence rather than "0 s ago",
 * which would claim a round trip that never happened (core/desk.ts
 * syncedAgoSeconds returns null for exactly this reason; collapsing it here
 * would throw that distinction away one layer later).
 */
export function syncedAgoText(seconds: number | null): string {
  if (seconds === null) return t("desk.synced.never");
  if (seconds < 10) return t("desk.synced.justNow");
  if (seconds < MINUTE_S) return t("desk.synced.seconds", { n: seconds });
  if (seconds < HOUR_S) return t("desk.synced.minutes", { n: Math.floor(seconds / MINUTE_S) });
  if (seconds < DAY_S) return t("desk.synced.hours", { n: Math.floor(seconds / HOUR_S) });
  return t("desk.synced.days", { n: Math.floor(seconds / DAY_S) });
}

/**
 * How long a tray item has been waiting, in the unit that carries the meaning.
 *
 * Rounded DOWN, always: "waiting 9 d" has to mean at least nine days, because
 * the number is being used as a nag and a nag that overstates itself is one
 * people learn to discount. Under an hour is not given a number at all —
 * something flagged twenty minutes ago is not waiting, it is in progress.
 */
export function waitText(ms: number): string {
  if (ms >= DAY_MS) return t("desk.wait.days", { n: Math.floor(ms / DAY_MS) });
  if (ms >= HOUR_MS) return t("desk.wait.hours", { n: Math.floor(ms / HOUR_MS) });
  return t("desk.wait.underHour");
}

/** "3 over a week" / "1 over a week" — two keys, because English needs two and
 * this app has no ICU library (i18n.ts). */
export function overWeekText(n: number): string {
  return n === 1 ? t("desk.tray.overWeek.one") : t("desk.tray.overWeek.other", { n });
}

/** "Show 50 more of 218": how many the button would fetch, and what it is a
 * bound on. The page size is passed in rather than imported so this module
 * stays free of the sync engine. */
export function moreText(remaining: number, page: number, total: number): string {
  return t("desk.section.more", { n: Math.min(page, remaining), total });
}

/**
 * The alias a message arrived at — the local part of `deliveredTo` with the
 * `@` kept, `invoices@`.
 *
 * The DOMAIN is dropped on purpose: on a catch-all account every row would
 * repeat the same domain, and the part that distinguishes one row from the next
 * is the part before the `@`. The `@` stays so the fragment still reads as an
 * address rather than as a word.
 *
 * "" for a message with no `deliveredTo` (and for a value with no `@` at all,
 * which is a header we cannot interpret) — the row then simply omits it, rather
 * than printing a stray glyph.
 */
export function aliasOf(deliveredTo: string | null | undefined): string {
  if (!deliveredTo) return "";
  const at = deliveredTo.lastIndexOf("@");
  if (at <= 0) return "";
  return `${deliveredTo.slice(0, at)}@`;
}

function sameDay(a: Date, b: Date): boolean {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}

/**
 * The time on a row: `09:14` today, `Yest.` yesterday, `12 Aug` this year,
 * `12 Aug 2025` before that.
 *
 * FOUR SHAPES, NOT ONE, because the useful part changes with age. Today, the
 * only thing worth knowing is the clock time; yesterday, the clock time is
 * noise and the word is the whole answer; older than that, the date. Rendering
 * a bare `08:14` on a message from March is the classic mail-client lie — it
 * reads as "this morning" at a glance.
 *
 * The month name comes from the runtime's locale; the ORDER is fixed
 * (day-then-month) rather than locale-derived, which is a deliberate limit:
 * `Intl` would give `Aug 12` in en-US, and this column is narrow enough that a
 * stable shape matters more than regional order until there is a second locale
 * to test against.
 */
export function rowTime(iso: string, now: number): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const nowDate = new Date(now);
  if (sameDay(d, nowDate)) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
  }
  const yesterday = new Date(now - DAY_MS);
  if (sameDay(d, yesterday)) return t("desk.time.yesterday");
  const month = d.toLocaleDateString(undefined, { month: "short" });
  if (d.getFullYear() === nowDate.getFullYear()) return `${d.getDate()} ${month}`;
  return `${d.getDate()} ${month} ${d.getFullYear()}`;
}

/**
 * A file size, as a person reads one: `812 B`, `40 KB`, `2.4 MB`.
 *
 * ⚠ THIS IS AN AGREEMENT, NOT A CONVENIENCE. The reading pane's attachment tile
 * and the viewer's top bar print the size of the SAME bytes, one above the other
 * in the same session, and two roundings of one number ("40 KB" against "39.7 KB")
 * read as two different files. It was ThreadPane's private helper until the viewer
 * needed the same sentence (#449); the composer's tiles still carry their own copy
 * for uploads, which is worth folding in the next time that file is open.
 *
 * Binary units with decimal-looking labels, which is what every mail client and
 * file manager shows; one decimal place from a megabyte up, because that is where
 * the difference between 2.4 and 2.9 starts to matter to whoever is about to
 * download it.
 */
export function bytesText(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** The line's threshold, always two decimals, so `0.6` and `0.60` are not two
 * different-looking answers to the same question and the number is never
 * hidden behind the words beside it. */
export function thresholdText(value: number): string {
  return value.toFixed(2);
}

/** A line being considered, and what the server said it would do — the third
 * clause of the slider's value text, or null when nothing is pending and there
 * is no third clause to make. */
export interface LinePendingMove {
  /** The pending threshold, already formatted (`thresholdText`). */
  threshold: string;
  /** How many messages would change sides. */
  moves: number;
  /** Which way the pending line is from the stored one. */
  direction: "stricter" | "looser";
}

/**
 * WHAT THE LINE IS WORTH SAYING OUT LOUD (#447) — the slider's `aria-valuetext`.
 *
 * The line is a `role="slider"` whose `aria-valuenow` is a triage threshold
 * between 0 and 1. A screen reader reading "0.55" out of that number would be
 * reading the one thing about the line that means least: nobody knows what 0.55
 * of a triage score is. What the number DOES is split the mail, so the value text
 * is the split — the same two counts the row prints, in words.
 *
 * ⚠ THE THIRD CLAUSE NAMES THE PENDING THRESHOLD RATHER THAN "ONE STEP", and that
 * is a deliberate correction to the wording the design sketched ("one step
 * stricter moves 2"). The pending line is not necessarily one step away: the two
 * buttons and the two arrow keys move it as often as they are pressed, and the
 * preview is a question about wherever it has got to. After two clicks, "one step
 * stricter moves 2" would be false about a line two steps away — and a slider that
 * misdescribes what it is about to do is worse than one that says less.
 *
 * `moves: 0` gets its own sentence for the same reason: "would move nothing" is a
 * useful answer (the step is too small to change anything here) and "would move 0"
 * reads like a failed measurement.
 */
export function lineValueText(
  review: number,
  drawers: number,
  pending: LinePendingMove | null,
): string {
  const counts = t("desk.line.value", { review, drawers });
  if (pending === null) return counts;
  if (pending.moves === 0) {
    return `${counts}; ${t("desk.line.moveNone", { threshold: pending.threshold })}`;
  }
  const clause =
    pending.direction === "stricter"
      ? t("desk.line.moveToDrawers", { threshold: pending.threshold, n: pending.moves })
      : t("desk.line.moveToSurface", { threshold: pending.threshold, n: pending.moves });
  return `${counts}; ${clause}`;
}


// ---------------------------------------------------------------------------
// The offline / stale header states (#375 increment 6)
// ---------------------------------------------------------------------------

/**
 * A duration in the unit — or the two units — that carry its meaning.
 *
 * TWO PARTS ABOVE AN HOUR, unlike `syncedAgoText` above, and the difference is
 * deliberate. "synced 3 h ago" is a reassurance and rounding it down understates
 * nothing that matters; "last synced 3 h ago" about an account that is actually
 * three hours and fifty minutes behind is the header understating the problem by
 * nearly an hour, in the one sentence someone is reading to decide whether to go
 * and look at their provider.
 *
 * Rounded DOWN in both parts, so the number is never a claim that the gap is
 * bigger than it is, and the smaller part is dropped when it is zero — "3 h
 * 0 min" is two numbers where one is an answer.
 */
export function durationText(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  if (s < MINUTE_S) return t("duration.seconds", { n: s });
  if (s < HOUR_S) return t("duration.minutes", { n: Math.floor(s / MINUTE_S) });
  if (s < DAY_S) {
    const h = Math.floor(s / HOUR_S);
    const m = Math.floor((s % HOUR_S) / MINUTE_S);
    return m === 0 ? t("duration.hours", { n: h }) : t("duration.hoursMinutes", { h, m });
  }
  const d = Math.floor(s / DAY_S);
  const h = Math.floor((s % DAY_S) / HOUR_S);
  return h === 0 ? t("duration.days", { n: d }) : t("duration.daysHours", { d, h });
}

/**
 * An RFC 3339 instant as a bare clock time — "09:02".
 *
 * A CLOCK, not a duration, and it is the one place on this screen that is:
 * "provider not reachable since 09:02" is a fact about a moment the user can
 * line up against their own morning, whereas everything else in the sentence
 * answers "how current is this" and therefore has to be a duration
 * (syncedAgoText's comment). 24-hour and locale-timed, matching `rowTime`.
 *
 * "" for an instant that does not parse, so the caller can drop the clause
 * rather than print "since Invalid Date".
 */
export function clockText(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
}

/**
 * The clauses the header appends for a connection state — and NOTHING for `ok`,
 * which is what keeps the ordinary sentence exactly as it was.
 *
 * ⚠ EVERY CLAUSE IS CONDITIONAL, because every field behind it is. An account
 * that has never synced gets "has never synced" rather than "0 s ago"; an
 * outage with no recorded time gets no "since"; a retry cadence nothing
 * reports gets no "retrying every". The decision about which facts exist was
 * already made in core/headerState.ts — this only refuses to invent the ones it
 * was not given (CLAUDE.md: a measurement is a claim about the code that
 * produced it).
 */
export function headerStateParts(state: HeaderState, now: number): string[] {
  switch (state.kind) {
    case "ok":
      return [];
    case "offline": {
      const parts = [t("header.offline.lead")];
      const seconds = syncedAgoSeconds(state.lastSyncAt, now);
      parts.push(
        seconds === null
          ? t("header.offline.neverSynced")
          : t("header.offline.lastSynced", { duration: durationText(seconds) }),
      );
      // Omitted at zero: "0 changes waiting to send" is a reassurance nobody
      // asked for, in a sentence that is already long.
      if (state.queued > 0) {
        parts.push(
          state.queued === 1
            ? t("header.offline.queued.one")
            : t("header.offline.queued.other", { n: state.queued }),
        );
      }
      return parts;
    }
    case "stale": {
      const parts = [t("header.stale.lead")];
      parts.push(
        state.lagSeconds === null
          ? t("header.stale.neverSynced", { account: state.account })
          : t("header.stale.lastSynced", {
              account: state.account,
              duration: durationText(state.lagSeconds),
            }),
      );
      if (state.since) {
        const clock = clockText(state.since);
        if (clock) parts.push(t("header.stale.since", { time: clock }));
      }
      if (state.retryEverySeconds !== undefined) {
        parts.push(t("header.stale.retry", { duration: durationText(state.retryEverySeconds) }));
      }
      return parts;
    }
    case "error":
      // The wording the header has always used for this. One fact, one sentence
      // — the message itself is in the section errors and the health panel.
      return [t("desk.header.unreachable")];
  }
}

/** The in-tray strip's shorter form of the same caveat: the counts on that line
 * were true at the last sync, and offline they are not being refreshed. */
export function asOfText(seconds: number | null): string {
  if (seconds === null) return t("header.offline.neverSynced");
  return t("header.offline.asOf", { duration: durationText(seconds) });
}

/**
 * WHAT A DRAWER'S DISCLOSURE BUTTON IS CALLED (#447).
 *
 * The button's visible word is "Open" / "Close", and until this issue that was
 * also its accessible name — so a reader tabbing down the Everything-else tab
 * heard "Open, button. Open, button. Open, button." once per area, with no way to
 * tell which drawer any of them belonged to. The name now carries the three facts
 * the row prints beside it: whose drawer, how much is in it, and what will happen
 * to it on its own.
 *
 * THE VISIBLE WORD STAYS. It is the shortest true label for a pointer, and the
 * state is on `aria-expanded` rather than in this string — a name that said "Open"
 * while `aria-expanded` said "expanded" would be two answers to one question.
 *
 * THE PERIOD IS SPELLED OUT HERE AND ABBREVIATED ON SCREEN ("30 days" against
 * "30 d"). Not an inconsistency: the row is a fixed-height line with a column to
 * fit, and "d" read aloud is the letter d.
 *
 * `periodDays: null` is "never sweeps" — and is ALSO what the caller passes while
 * the preferences have not loaded (core/desk.ts drawerPeriodDays). This says
 * exactly what the visible line says in that case rather than inventing a third
 * sentence for a state the row does not distinguish either.
 */
export function drawerButtonLabel(
  area: string,
  count: number,
  periodDays: number | null,
  sweepEnabled: boolean,
): string {
  const period = periodDays === null ? null : tCount("desk.drawer.sweepDays", periodDays);
  const sweep =
    period === null
      ? t("desk.drawer.noSweep")
      : // "sweep off" is APPENDED rather than replacing the period, exactly as the
        // visible line appends it: the period is still this area's setting and
        // still what would happen if the master switch were on, and hiding it
        // would make the drawer sound unconfigured.
        sweepEnabled
        ? period
        : `${period}, ${t("desk.drawer.sweepOff")}`;
  return tCount("desk.drawer.button", count, { area, sweep });
}
