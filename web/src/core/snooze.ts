// SPDX-License-Identifier: AGPL-3.0-only

// Snooze presets (spec §6, #51): the handful of "when should this come back"
// answers that cover almost every snooze, resolved against the LOCAL clock.
// DOM-free and framework-agnostic (spec §12), like the rest of core.
//
// Two rules shape this list:
//
//   * A preset that has already passed is not offered. "This evening" at 9pm is
//     not a choice, it is a trap — the server refuses a wake time in the past
//     (internal/api/email.go), and rightly, so the UI must never present one.
//   * The resolved time is carried alongside the label, so the UI can show
//     exactly what "next week" means before the user commits. A snooze is a
//     promise about the future; a client that is vague about which future is
//     asking to be mistrusted.
//
// Times are LOCAL — 8am means 8am where the user is — and are converted to an
// absolute instant (ISO 8601, UTC) at the moment the user picks one, not when
// the offline queue happens to drain.

/** The stable identity of a preset; the UI maps it to a translated label. */
export type SnoozePresetKey = "laterToday" | "thisEvening" | "tomorrow" | "thisWeekend" | "nextWeek";

export interface SnoozePreset {
  key: SnoozePresetKey;
  /** The absolute instant this preset resolves to. */
  at: Date;
}

/** Morning, for "tomorrow" and later. */
const MORNING_HOUR = 8;
/** Evening, for "this evening". */
const EVENING_HOUR = 18;
/** "Later today" is a nudge, not a reschedule. */
const LATER_TODAY_HOURS = 3;

function at(base: Date, addDays: number, hour: number): Date {
  const d = new Date(base);
  d.setDate(d.getDate() + addDays);
  d.setHours(hour, 0, 0, 0);
  return d;
}

/** Days until the next Saturday (never 0: "this weekend" on a Saturday means the
 * one coming, not the moment you are standing in). */
function daysToWeekend(base: Date): number {
  const saturday = 6;
  const delta = (saturday - base.getDay() + 7) % 7;
  return delta === 0 ? 7 : delta;
}

/** Days until the next Monday, same rule. */
function daysToNextWeek(base: Date): number {
  const monday = 1;
  const delta = (monday - base.getDay() + 7) % 7;
  return delta === 0 ? 7 : delta;
}

/** The presets worth offering right now, in the order they should be shown. */
export function snoozePresets(now: Date = new Date()): SnoozePreset[] {
  const laterToday = new Date(now.getTime() + LATER_TODAY_HOURS * 60 * 60 * 1000);
  const candidates: SnoozePreset[] = [
    { key: "laterToday", at: laterToday },
    { key: "thisEvening", at: at(now, 0, EVENING_HOUR) },
    { key: "tomorrow", at: at(now, 1, MORNING_HOUR) },
    { key: "thisWeekend", at: at(now, daysToWeekend(now), MORNING_HOUR) },
    { key: "nextWeek", at: at(now, daysToNextWeek(now), MORNING_HOUR) },
  ];
  // Drop anything already past, and anything a nearer preset has overtaken —
  // "later today" at 4pm and "this evening" at 6pm are two useful choices, but
  // "later today" at 5:30pm and "this evening" at 6pm are the same answer twice.
  const out: SnoozePreset[] = [];
  for (const p of candidates) {
    if (p.at.getTime() <= now.getTime()) continue;
    const prev = out[out.length - 1];
    if (prev && p.at.getTime() - prev.at.getTime() < 60 * 60 * 1000) {
      out[out.length - 1] = p;
      continue;
    }
    out.push(p);
  }
  return out;
}

/** Converts a datetime-local input value ("2026-07-26T08:00") into an absolute
 * ISO instant, or null when it is unparseable or not in the future. The server
 * enforces the same rule; checking here is what lets the UI say so kindly
 * instead of surfacing a rejected write. */
export function customSnoozeTime(value: string, now: Date = new Date()): string | null {
  if (!value) return null;
  const at = new Date(value);
  if (Number.isNaN(at.getTime()) || at.getTime() <= now.getTime()) return null;
  return at.toISOString();
}

/** The `min` a datetime-local picker should carry: one minute from now, in the
 * local-time spelling the input expects. */
export function earliestSnoozeInput(now: Date = new Date()): string {
  const d = new Date(now.getTime() + 60 * 1000);
  const pad = (n: number): string => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** "Returns Tue 8:00 AM" — how a wake time is spelled in a list row or a banner.
 * Same-day wakes drop the date, distant ones keep it: the useful information is
 * how far away it is, not the full timestamp. */
export function formatWake(iso: string, now: Date = new Date()): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  const time = at.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
  const sameDay =
    at.getFullYear() === now.getFullYear() &&
    at.getMonth() === now.getMonth() &&
    at.getDate() === now.getDate();
  if (sameDay) return time;
  const withinAWeek = at.getTime() - now.getTime() < 7 * 24 * 60 * 60 * 1000;
  const date = withinAWeek
    ? at.toLocaleDateString(undefined, { weekday: "short" })
    : at.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  return `${date} ${time}`;
}
