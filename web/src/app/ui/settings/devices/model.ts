// SPDX-License-Identifier: AGPL-3.0-only

// Pure helpers for the signed-in devices row (#937).

import type { SignedInDevice } from "../../../../core/jmap/types";
import { keyDateLong } from "../../../../core/userKeys";
import { t } from "../../../i18n";

/** This device first, then the most recently active. Does not mutate. */
export function sortDevices(list: readonly SignedInDevice[]): SignedInDevice[] {
  return [...list].sort((a, b) => {
    if (a.isCurrent !== b.isCurrent) return a.isCurrent ? -1 : 1;
    return Date.parse(b.lastSeenAt) - Date.parse(a.lastSeenAt);
  });
}

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

/** "just now", "5 min ago", "3 h ago", "2 d ago"; a date from two weeks on
 * (or, for a time that cannot be read, the raw text). */
export function lastActiveText(iso: string, now: number): string {
  const at = Date.parse(iso);
  if (Number.isNaN(at)) return iso;
  const ago = Math.max(0, now - at);
  if (ago < MINUTE_MS) return t("devices.ago.now");
  if (ago < HOUR_MS) return t("devices.ago.minutes", { n: Math.floor(ago / MINUTE_MS) });
  if (ago < DAY_MS) return t("devices.ago.hours", { n: Math.floor(ago / HOUR_MS) });
  if (ago < 14 * DAY_MS) return t("devices.ago.days", { n: Math.floor(ago / DAY_MS) });
  return keyDateLong(iso);
}
