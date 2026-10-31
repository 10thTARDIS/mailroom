// SPDX-License-Identifier: AGPL-3.0-only

// THE INVITATION CARD'S WORDS (#725), away from the JSX and under test.
//
// Everything the card says is decided here from the server's Email.calendarInvite
// — which the SERVER parsed (internal/ical); this client never sees a raw .ics.
// Two judgements live here and nowhere else:
//
//   * TIME. An instant is shown in the READER's zone, with the zone named, so
//     "15:00" is never ambiguous. A FLOATING time — one the server could not
//     place in any zone — is shown AS WRITTEN with the zone it was written in
//     named, and never converted: there is nothing to convert from, and showing
//     it in the reader's zone would be stating a fact nobody knows.
//   * STATE. Cancelled beats everything; then "there is a newer version";
//     then "this updates an earlier one". One line, the most important one.

import type {
  CalendarAttendee,
  CalendarInvite,
  CalendarPartStat,
  CalendarTime,
} from "../../../core/jmap/types";
import { stripBidiControls } from "../../../core/hostileText";
import { t } from "../../i18n";
import type { MessageKey } from "../../i18n";

export interface FormatOptions {
  /** BCP 47 locale; the browser's when absent. */
  locale?: string;
  /** IANA zone to show instants in; the browser's (the reader's) when absent.
   * Tests pass one so they do not depend on the machine they run on. */
  timeZone?: string;
}

const DATE: Intl.DateTimeFormatOptions = { weekday: "short", day: "numeric", month: "short", year: "numeric" };
const TIME: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit" };

function fmt(d: Date, o: Intl.DateTimeFormatOptions, opts: FormatOptions, zone?: string): string {
  return new Intl.DateTimeFormat(opts.locale, { ...o, timeZone: zone ?? opts.timeZone }).format(d);
}

/** "2026-10-20" or "2026-10-20T09:00:00" read as a wall clock (UTC-anchored,
 * and formatted in UTC, so no zone ever shifts it). */
function wall(s: string): Date | null {
  const d = new Date(s.length === 10 ? `${s}T00:00:00Z` : `${s}Z`);
  return Number.isNaN(d.getTime()) ? null : d;
}

function dayKey(d: Date, opts: FormatOptions, zone?: string): string {
  return fmt(d, { year: "numeric", month: "2-digit", day: "2-digit" }, opts, zone);
}

/**
 * When the event is, as one line. Null when the invitation names no start.
 */
export function formatWhen(
  start: CalendarTime | null | undefined,
  end: CalendarTime | null | undefined,
  opts: FormatOptions = {},
): string | null {
  if (!start) return null;
  if (start.kind === "utc" && start.utc) {
    const s = new Date(start.utc);
    if (Number.isNaN(s.getTime())) return null;
    const e = end?.kind === "utc" && end.utc ? new Date(end.utc) : null;
    const zoned = { ...TIME, timeZoneName: "short" } as const;
    if (!e || Number.isNaN(e.getTime()) || e.getTime() <= s.getTime()) {
      return `${fmt(s, DATE, opts)}, ${fmt(s, zoned, opts)}`;
    }
    if (dayKey(s, opts) === dayKey(e, opts)) {
      return `${fmt(s, DATE, opts)}, ${fmt(s, TIME, opts)} – ${fmt(e, zoned, opts)}`;
    }
    return `${fmt(s, DATE, opts)}, ${fmt(s, TIME, opts)} – ${fmt(e, DATE, opts)}, ${fmt(e, zoned, opts)}`;
  }
  if (start.kind === "date" && start.date) {
    const s = wall(start.date);
    if (!s) return null;
    // DTEND of an all-day event is EXCLUSIVE (RFC 5545 §3.6.1): a one-day
    // event on the 24th ends on the 25th.
    const e = end?.kind === "date" && end.date ? wall(end.date) : null;
    const last = e ? new Date(e.getTime() - 86_400_000) : null;
    const first = fmt(s, DATE, opts, "UTC");
    const range = last && last.getTime() > s.getTime() ? `${first} – ${fmt(last, DATE, opts, "UTC")}` : first;
    return t("invite.allDay", { date: range });
  }
  if (start.local) {
    const s = wall(start.local);
    if (!s) return null;
    const e = end?.kind === "floating" && end.local ? wall(end.local) : null;
    let text = `${fmt(s, DATE, opts, "UTC")}, ${fmt(s, TIME, opts, "UTC")}`;
    if (e && e.getTime() > s.getTime()) {
      text +=
        dayKey(s, opts, "UTC") === dayKey(e, opts, "UTC")
          ? ` – ${fmt(e, TIME, opts, "UTC")}`
          : ` – ${fmt(e, DATE, opts, "UTC")}, ${fmt(e, TIME, opts, "UTC")}`;
    }
    const zone = start.timeZone ? stripBidiControls(start.timeZone) : "";
    return zone ? t("invite.floatingZone", { time: text, zone }) : t("invite.floatingNoZone", { time: text });
  }
  return null;
}

const ORDINAL: Record<string, MessageKey> = {
  "1": "invite.ordinal.1",
  "+1": "invite.ordinal.1",
  "2": "invite.ordinal.2",
  "+2": "invite.ordinal.2",
  "3": "invite.ordinal.3",
  "+3": "invite.ordinal.3",
  "4": "invite.ordinal.4",
  "+4": "invite.ordinal.4",
  "-1": "invite.ordinal.last",
};

// 4 Jan 2026 is a Sunday: day-of-week names come from Intl, so they are in the
// reader's language without a catalogue of our own.
const DAY_INDEX: Record<string, number> = { SU: 0, MO: 1, TU: 2, WE: 3, TH: 4, FR: 5, SA: 6 };
function dayName(code: string, opts: FormatOptions): string {
  const i = DAY_INDEX[code];
  if (i === undefined) return code;
  return fmt(new Date(Date.UTC(2026, 0, 4 + i)), { weekday: "long" }, opts, "UTC");
}

/** "Repeats weekly on Monday, Wednesday, 10 times" — from the server's summary,
 * never from an expansion (there is none). */
export function repeatsText(rec: CalendarInvite["recurrence"], opts: FormatOptions = {}): string | null {
  if (!rec) return null;
  const freq = rec.freq ?? "";
  const known = ["DAILY", "WEEKLY", "MONTHLY", "YEARLY"].includes(freq);
  if (!known) return t("invite.repeats.generic");
  let base =
    rec.interval > 1
      ? t("invite.repeats.every", { interval: rec.interval, unit: t(`invite.unit.${freq}` as MessageKey) })
      : t(`invite.repeats.${freq}` as MessageKey);
  if (rec.byDay.length > 0) {
    const days = rec.byDay.map((d) => {
      const code = d.slice(-2);
      const n = d.slice(0, -2);
      const name = dayName(code, opts);
      const ord = n ? ORDINAL[n] : undefined;
      return ord ? `${t(ord)} ${name}` : name;
    });
    base = t("invite.repeats.onDays", { base, days: days.join(", ") });
  }
  if (rec.count) return t("invite.repeats.count", { base, count: rec.count });
  if (rec.until) {
    const until = formatUntil(rec.until, opts);
    if (until) return t("invite.repeats.until", { base, date: until });
  }
  return base;
}

function formatUntil(u: CalendarTime, opts: FormatOptions): string | null {
  if (u.kind === "utc" && u.utc) return fmt(new Date(u.utc), DATE, opts);
  const w = wall(u.date ?? u.local ?? "");
  return w ? fmt(w, DATE, opts, "UTC") : null;
}

/** Who someone is, for the card: the name when the invitation gave one, the
 * address otherwise. Bidi-stripped — these are sender-authored strings in OUR
 * DOM (docs/hostile-content.md §2.3b). */
export function personLabel(p: { name: string | null; email: string } | null | undefined): string {
  if (!p) return "";
  const name = stripBidiControls(p.name ?? "").trim();
  const email = stripBidiControls(p.email);
  return name && name !== email ? `${name} <${email}>` : email;
}

export function partstatLabel(ps: CalendarPartStat): string {
  return t(`invite.partstat.${ps}` as MessageKey);
}

/** The one state line the card leads with, or null. */
export type InviteState = "cancelled" | "newer" | "updates" | "updated" | null;

export function inviteState(inv: CalendarInvite): InviteState {
  if (inv.cancelled) return "cancelled";
  if (inv.newerEmailId) return "newer";
  if (inv.updatesEmailId) return "updates";
  // SEQUENCE > 0 IS an update by definition (RFC 5546 §2.1.4), even when the
  // earlier message is not in this mailbox or predates the index.
  if ((inv.sequence ?? 0) > 0 && inv.method === "REQUEST") return "updated";
  return null;
}

/**
 * The "could not be verified" line, or null (#725 review). Only for a message
 * that CLAIMS to change something — a cancellation, an update (SEQUENCE > 0) or
 * a reply. A first invitation that is merely unauthenticated says nothing:
 * on an instance with no trusted authserv-id that would be every invitation,
 * and a warning on everything teaches the reader to ignore it.
 */
export function unverifiedText(inv: CalendarInvite): string | null {
  if (inv.verified !== false) return null;
  if (inv.method === "REPLY") return t("invite.unverified.reply");
  if (inv.method === "CANCEL" || inv.status === "CANCELLED" || (inv.sequence ?? 0) > 0) {
    return t("invite.unverified.update");
  }
  return null;
}

/** The sentence for the user's own recorded answer, or null. */
export function responseText(inv: CalendarInvite): string | null {
  const r = inv.response;
  if (!r) return null;
  const key: MessageKey =
    r.partstat === "ACCEPTED"
      ? "invite.you.ACCEPTED"
      : r.partstat === "TENTATIVE"
        ? "invite.you.TENTATIVE"
        : "invite.you.DECLINED";
  const said = t(key);
  // An answer to an EARLIER version: the organiser has changed something
  // since, and the honest card says so rather than claiming the answer stands.
  if (r.sequence < (inv.sequence ?? 0)) return t("invite.you.earlier", { answer: said });
  return said;
}

/** For a REPLY message: who answered, and how. */
export function replySummary(attendees: CalendarAttendee[] | undefined): string[] {
  return (attendees ?? []).map((a) => {
    const who = personLabel(a);
    switch (a.partstat) {
      case "ACCEPTED":
        return t("invite.replied.ACCEPTED", { who });
      case "TENTATIVE":
        return t("invite.replied.TENTATIVE", { who });
      case "DECLINED":
        return t("invite.replied.DECLINED", { who });
      default:
        return t("invite.replied.other", { who });
    }
  });
}
