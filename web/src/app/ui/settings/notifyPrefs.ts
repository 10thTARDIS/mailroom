// SPDX-License-Identifier: AGPL-3.0-only

// The arithmetic behind Settings › Notifications (#59), kept out of the
// components so each rule has a test of its own: minutes after midnight and
// the `HH:MM` a time input speaks, the week the quiet-hours form shows, the
// zone it starts from, and the state line's words.

import type { PushMode, QuietHours } from "../../../core/jmap/types";
import { t } from "../../i18n";

/** Minutes after midnight → `HH:MM`, the value an `<input type="time">` takes. */
export function minutesToTime(m: number): string {
  const clamped = Math.min(Math.max(Math.trunc(m), 0), 1439);
  const h = Math.floor(clamped / 60);
  const mm = clamped % 60;
  return `${String(h).padStart(2, "0")}:${String(mm).padStart(2, "0")}`;
}

/** `HH:MM` → minutes after midnight, or null for anything a time input would
 * not produce (an empty field while the user is still typing, say). */
export function timeToMinutes(s: string): number | null {
  const m = /^(\d{1,2}):(\d{2})$/.exec(s.trim());
  if (!m) return null;
  const h = Number(m[1]);
  const mm = Number(m[2]);
  if (h > 23 || mm > 59) return null;
  return h * 60 + mm;
}

/** The week as the form lists it, Monday first, with the server's day numbers
 * (0 = Sunday — Go's time.Weekday, internal/notify). */
export const WEEK: ReadonlyArray<{ day: number; key: "mon" | "tue" | "wed" | "thu" | "fri" | "sat" | "sun" }> = [
  { day: 1, key: "mon" },
  { day: 2, key: "tue" },
  { day: 3, key: "wed" },
  { day: 4, key: "thu" },
  { day: 5, key: "fri" },
  { day: 6, key: "sat" },
  { day: 0, key: "sun" },
];

/** The browser's own IANA zone, or "UTC" when it will not say. */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** The zones the form offers: the runtime's list when it has one, always
 * including the stored zone and the browser's, sorted. */
export function timeZoneChoices(stored: string, browser: string): string[] {
  let all: string[] = [];
  const intl = Intl as unknown as { supportedValuesOf?: (k: string) => string[] };
  if (typeof intl.supportedValuesOf === "function") {
    try {
      all = intl.supportedValuesOf("timeZone");
    } catch {
      all = [];
    }
  }
  return [...new Set([...all, stored, browser, "UTC"])].filter((z) => z !== "").sort();
}

/**
 * The form's starting values. A setting that has never been switched on still
 * holds the server's default zone, UTC — which is nobody's wall clock — so the
 * form starts from the browser's zone instead; saving then stores it. A
 * setting the user HAS saved keeps the zone they chose, whatever this browser
 * thinks.
 */
export function initialQuietHours(stored: QuietHours, browser: string): QuietHours {
  if (!stored.enabled && stored.timeZone === "UTC") return { ...stored, timeZone: browser };
  return stored;
}

/** Why the form cannot be saved as it stands, or null. */
export function quietHoursProblem(q: QuietHours): string | null {
  if (q.start === q.end) return t("settings.quiet.sameTimes");
  if (q.enabled && q.days.length === 0) return t("settings.quiet.noDays");
  return null;
}

/** The state line's clause: off, or `quiet 22:00–07:00`. */
export function quietHoursFact(q: QuietHours | undefined): string | null {
  if (!q) return null;
  if (!q.enabled) return t("settings.state.quietOff");
  return t("settings.state.quietOn", { start: minutesToTime(q.start), end: minutesToTime(q.end) });
}

/** The four device modes, in the order the form lists them. */
export const PUSH_MODES: readonly PushMode[] = ["all", "above_line", "vip", "above_line_or_vip"];
