// SPDX-License-Identifier: AGPL-3.0-only

// THE TWO FACTS THAT MAKE THE LINE A DECISION RATHER THAN A DIAL (#374 increment 3).
//
// The desk already lets a user move the line and preview the result (ui/desk/TheLine.tsx).
// What it cannot say is whether the line they are on is the right one, and that
// question needs history and a derivative:
//
//   last 7 days: 28 to review · 289 to drawers
//   one step stricter would move 12 to drawers
//
// The first is what the current line HAS DONE — two `Email/query` counts over the
// same week, split by which side of the line the mail landed on. The second is
// what one click would cost, measured against the desk as it is.
//
// BOTH ARE MEASUREMENTS, SO BOTH CAN BE ABSENT. Neither sentence has a
// zero-shaped fallback: "last 7 days: 0 to review" is a claim about a week nobody
// counted, and it reads exactly like a quiet week (#159/#176). The row omits the
// sentence rather than printing a number it did not get, which is why both
// functions here return null on missing input instead of 0.
//
// PURE, so the arithmetic that decides what the user is told is testable without a
// server: the SIGN of `stricterDelta` is the whole content of its sentence, and a
// sign error is a sentence that is exactly backwards while looking perfectly
// reasonable.

import { headerCounts } from "../../../core/desk";
import type { AreaSummaryRow } from "../../../core/jmap/types";

/** A day in milliseconds. */
const DAY_MS = 24 * 60 * 60 * 1000;

/** How many days back the two counts look. Seven, because that is the shortest
 * window in which a weekday-shaped mail habit is a whole cycle — a three-day
 * window on a Monday is a different question than the same window on a Thursday. */
export const LINE_WINDOW_DAYS = 7;

/**
 * The line a FRESH login starts on, from `db/schema.sql` (`line_threshold real
 * NOT NULL DEFAULT 0.5`).
 *
 * Hard-coded because no method asks a server "what would a new user's line have
 * been": `DeskPreferences/get` answers with THIS user's line, which is the number
 * the reset is measured against rather than the number it restores. It is used for
 * one thing only — deciding whether to print `changed`, and what the reset offers.
 */
export const LINE_DEFAULT_THRESHOLD = 0.5;

/**
 * The `Email/query` filter bound for "the last seven days".
 *
 * ⚠ SEVEN TIMES TWENTY-FOUR HOURS, NOT SEVEN CALENDAR DAYS, and the sentence says
 * "last 7 days" for that reading. A calendar-day window would change length at
 * midnight and by timezone, and the number under it would move without any mail
 * arriving.
 *
 * `after` is INCLUSIVE on `receivedAt` (types.ts EmailFilter) and it is the only
 * bound sent: a `before: now` would exclude nothing — mail does not arrive from the
 * future — while adding a second value that goes stale between the two queries.
 *
 * The string is a JMAP UTCDate. `toISOString` includes milliseconds and the server
 * accepts them (internal/api/search.go jmapTime falls back to RFC3339, which is
 * what core/snooze.ts has always relied on), but they are trimmed anyway: the
 * canonical form in RFC 8620 §1.4 has no fraction, and a value that is canonical
 * cannot be refused by a stricter parser later.
 */
export function sevenDayWindow(now: number): { after: string } {
  const at = new Date(now - LINE_WINDOW_DAYS * DAY_MS);
  return { after: at.toISOString().replace(/\.\d{3}Z$/, "Z") };
}

/**
 * How much mail one step stricter would move OFF the surface and into drawers.
 *
 * ⚠ THE SIGN. Stricter means a HIGHER threshold, which keeps less on the surface,
 * so the answer is `current review − preview review` — a positive number is mail
 * that would leave the surface. Subtracting the other way round produces a
 * negative number that renders as a perfectly plausible sentence about mail moving
 * the wrong way, which is why the direction is asserted in the test rather than
 * left to the reader of this comment.
 *
 * ONLY THE REVIEW SIDE IS SUBTRACTED, not the drawer side, and they are not
 * interchangeable: the Needs-sorting row contributes to `review` and to no
 * drawer at all (core/desk.ts headerCounts says why), so the drawer difference
 * would answer a slightly different question and disagree with this one by
 * whatever unsorted mail crossed the line.
 *
 * Null when either side is missing — a preview that failed, or a desk whose
 * summary has not landed. Not 0: "nothing would move" and "nobody asked" are
 * different states, and only one of them is worth a sentence.
 */
export function stricterDelta(
  current: readonly AreaSummaryRow[] | null,
  preview: readonly AreaSummaryRow[] | null,
): number | null {
  if (current === null || preview === null) return null;
  return headerCounts([...current]).review - headerCounts([...preview]).review;
}
