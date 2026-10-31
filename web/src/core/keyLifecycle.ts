// SPDX-License-Identifier: AGPL-3.0-only

// The v1.93 final scope's client logic (#815, #813, #812, #817, #818), apart
// from any UI: an exact expiry date and what it maps to on the wire, a key's
// user IDs as a title, which of your keys are history, what a correspondent
// key's history and a refresh say, and what a failed replace left behind.
//
// ⚠ A CALENDAR DATE IS NOT AN INSTANT. The date the user picks in
// `<input type="date">` is a day in THEIR calendar ("2026-11-04"), and it goes
// to the server verbatim; the server reads it as the END of that day in UTC
// (23:59:59Z, ADR-040 §12). Nothing here turns it into a Date and back to a
// string through the local timezone: `new Date("2026-11-04")` is UTC midnight,
// which a UTC− browser prints as 3 Nov, and `toISOString()` of a local evening
// in a UTC− zone is already tomorrow. Both are off-by-one bugs a picker with
// a timezone in the loop makes silently, so the arithmetic here is on
// Date.UTC calendar days and the "today" it starts from is the local one.
//
// ⚠ AN ABSENT FACT IS NOT A DATE. A key retired before v1.93 has no recorded
// retirement time (`retiredAt` null). It is "retired (date not recorded)",
// never "retired on" something this client made up (#815).

import { JmapMethodError } from "./jmap/client";
import type { KeyActionError } from "./userKeys";
import type {
  ContactKeyEvent,
  ContactKeyRefreshResult,
  HeldContactKey,
  Id,
  KeyExpiryArg,
  KeyUserId,
  UserKey,
} from "./jmap/types";

const DAY_MS = 24 * 60 * 60 * 1000;

/** The server's cap on a key's lifetime (MaxKeyLifetimeDays). */
const MAX_DAYS = 3650;

// ---------------------------------------------------------------------------
// Exact expiry (#813)
// ---------------------------------------------------------------------------

const DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

/** The user's own calendar date for an instant, as `YYYY-MM-DD` (local time:
 * this is what "today" and "tomorrow" mean to the person picking). */
export function localDateString(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${String(y).padStart(4, "0")}-${m}-${day}`;
}

/** The calendar fields of a `YYYY-MM-DD`, or null when it is not a real date
 * (2026-02-30 is refused, not rolled over into March). */
function parseDate(date: string): { y: number; m: number; d: number } | null {
  const match = DATE_RE.exec(date.trim());
  if (!match) return null;
  const y = Number(match[1]);
  const m = Number(match[2]);
  const d = Number(match[3]);
  const back = new Date(Date.UTC(y, m - 1, d));
  if (back.getUTCFullYear() !== y || back.getUTCMonth() !== m - 1 || back.getUTCDate() !== d) return null;
  return { y, m, d };
}

/** A calendar date plus `n` days, as a calendar date (no timezone involved). */
export function addCalendarDays(date: string, n: number): string {
  const p = parseDate(date);
  if (!p) return date;
  const t = new Date(Date.UTC(p.y, p.m - 1, p.d) + n * DAY_MS);
  return t.toISOString().slice(0, 10);
}

/** The instant the server will put on the key for a picked date: 23:59:59 UTC
 * that day (ADR-040 §12). Null for something that is not a date. */
export function endOfUtcDay(date: string): number | null {
  const p = parseDate(date);
  if (!p) return null;
  return Date.UTC(p.y, p.m - 1, p.d, 23, 59, 59);
}

/** Why a picked date cannot be sent, or null when the server will take it. */
export type ExactDateProblem = "invalid" | "tooSoon" | "tooFar";

/**
 * The server's own rule (parseExpiresAt): the end of the day must be at least
 * 24 hours and at most 3650 days from now. Checked against the instant the
 * server will compute, so a date the page offers is a date the server accepts.
 */
export function exactDateProblem(date: string, now: Date): ExactDateProblem | null {
  const at = endOfUtcDay(date);
  if (at === null) return "invalid";
  const left = at - now.getTime();
  if (left < DAY_MS) return "tooSoon";
  if (left > MAX_DAYS * DAY_MS) return "tooFar";
  return null;
}

/**
 * The picker's `min` and `max`: from tomorrow (in the user's calendar) to ten
 * years out, each moved inward until the server's rule holds. Late in the
 * evening in a UTC− zone, "tomorrow" already ends less than 24 hours from now
 * in UTC terms, so the first date offered is the day after.
 */
export function exactDateBounds(now: Date): { min: string; max: string } {
  let min = addCalendarDays(localDateString(now), 1);
  for (let i = 0; i < 3 && exactDateProblem(min, now) === "tooSoon"; i++) min = addCalendarDays(min, 1);
  let max = new Date(now.getTime() + MAX_DAYS * DAY_MS).toISOString().slice(0, 10);
  for (let i = 0; i < 3 && exactDateProblem(max, now) === "tooFar"; i++) max = addCalendarDays(max, -1);
  return { min, max };
}

/** Whole calendar days from the user's today to the picked date. */
export function calendarDaysFromToday(date: string, now: Date): number | null {
  const p = parseDate(date);
  const today = parseDate(localDateString(now));
  if (!p || !today) return null;
  return Math.round((Date.UTC(p.y, p.m - 1, p.d) - Date.UTC(today.y, today.m - 1, today.d)) / DAY_MS);
}

/** "4 Nov 2026": the picked DAY itself, printed in UTC so no timezone can move
 * it to the 3rd or the 5th. */
export function calendarDateLong(date: string): string {
  const p = parseDate(date);
  if (!p) return date;
  return new Date(Date.UTC(p.y, p.m - 1, p.d)).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
    timeZone: "UTC",
  });
}

/** Whether a key made to expire on this date is inside the server's warning
 * window from the start (it shows "Expires soon" at once). No threshold, no
 * judgement. */
export function exactDateExpiresSoon(date: string, now: Date, warnDays: number | null): boolean {
  const at = endOfUtcDay(date);
  if (at === null || warnDays === null) return false;
  return at - now.getTime() < warnDays * DAY_MS;
}

/** The expiry choices of generate and replace. */
export type NewKeyExpiry = "1y" | "2y" | "5y" | "never" | "date";

/**
 * What generate and replace send for a choice: the server's default for two
 * years (no argument at all), days for the other presets, 0 for never, and the
 * picked date verbatim for "On a date…". Null when the date cannot be sent.
 */
export function newKeyExpiryArg(choice: NewKeyExpiry, date: string, now: Date): KeyExpiryArg | null {
  switch (choice) {
    case "1y":
      return { expiresDays: 365 };
    case "2y":
      return {};
    case "5y":
      return { expiresDays: 5 * 365 };
    case "never":
      return { expiresDays: 0 };
    case "date":
      return exactDateProblem(date, now) === null ? { expiresAt: date.trim() } : null;
  }
}

// ---------------------------------------------------------------------------
// User IDs (#812)
// ---------------------------------------------------------------------------

/** "Name <email>", or whichever half there is. Plain text: the caller renders
 * it as a text node, never as markup. */
export function userIdText(u: Pick<KeyUserId, "name" | "email">): string {
  const name = u.name.trim();
  const email = u.email.trim();
  if (name && email) return `${name} <${email}>`;
  return name || email;
}

/** The key's title and the rest: the primary user ID (the server sends it
 * first; a `primary` flag elsewhere wins) and every other one, in order. Null
 * title when the key lists none (or the server is older): the caller falls
 * back to what it said before. Empty user IDs are skipped. */
export function titleUserIds(userIds: readonly KeyUserId[] | undefined): {
  title: KeyUserId | null;
  others: KeyUserId[];
} {
  const list = (userIds ?? []).filter((u) => userIdText(u) !== "");
  if (list.length === 0) return { title: null, others: [] };
  const i = Math.max(
    0,
    list.findIndex((u) => u.primary),
  );
  const title = list[i] as KeyUserId;
  return { title, others: list.filter((_, j) => j !== i) };
}

// ---------------------------------------------------------------------------
// Your keys' history (#815)
// ---------------------------------------------------------------------------

/**
 * Whether one of your keys belongs under History rather than in the list of
 * keys in use: retired, revoked, or expired and used for no address. An
 * expired key that is still bound stays in the list, because Extend and
 * Replace are what it needs.
 */
export function isPastKey(k: Pick<UserKey, "decryptOnly" | "revokedAt" | "expired" | "identityId">): boolean {
  return k.revokedAt !== null || k.decryptOnly || (k.expired === true && k.identityId === null);
}

/** What the page says about when a key was retired. */
export type RetiredFact = { kind: "none" } | { kind: "on"; at: string } | { kind: "notRecorded" };

/**
 * `retiredAt` read honestly:
 *   - a key in use: nothing;
 *   - a recorded time: "Retired on …", except when it is the revocation itself
 *     (revoke records both at once; the revoked line already says it);
 *   - no recorded time on a retired key: "Retired (date not recorded)";
 *   - no recorded time on a revoked key: nothing beyond "Revoked on …",
 *     because whether it was retired first is not known either.
 */
export function retiredFact(k: Pick<UserKey, "decryptOnly" | "revokedAt" | "retiredAt">): RetiredFact {
  if (!k.decryptOnly && k.revokedAt === null) return { kind: "none" };
  const at = k.retiredAt ?? null;
  if (at === null) return k.revokedAt !== null ? { kind: "none" } : { kind: "notRecorded" };
  if (k.revokedAt !== null) {
    const a = new Date(at).getTime();
    const r = new Date(k.revokedAt).getTime();
    // The two are written by one revoke, a moment apart at most.
    if (!Number.isNaN(a) && !Number.isNaN(r) && Math.abs(a - r) < 60_000) return { kind: "none" };
  }
  return { kind: "on", at };
}

// ---------------------------------------------------------------------------
// Correspondents' keys: history (#815)
// ---------------------------------------------------------------------------

/** The kinds of event the page words; anything else is `other`. */
export type KeyEventKind =
  | "first_seen"
  | "renewed"
  | "updated"
  | "revoked_seen"
  | "pinned"
  | "refused"
  | "removed"
  | "other";

/** Where an event came from, as the page words it. */
export type KeyEventSource = "autocrypt" | "wkd" | "manual" | "keyserver" | "user" | "other";

const EVENT_KINDS: readonly string[] = ["first_seen", "renewed", "updated", "revoked_seen", "pinned", "refused", "removed"];
const EVENT_SOURCES: readonly string[] = ["autocrypt", "wkd", "manual", "keyserver", "user"];

export interface KeyHistoryLine {
  kind: KeyEventKind;
  source: KeyEventSource;
  at: string;
  expiresAt: string | null;
}

/** The server's events in a closed set, oldest first as sent. */
export function keyHistoryLines(events: readonly ContactKeyEvent[]): KeyHistoryLine[] {
  return events.map((e) => ({
    kind: EVENT_KINDS.includes(e.kind) ? (e.kind as KeyEventKind) : "other",
    source: EVENT_SOURCES.includes(e.source) ? (e.source as KeyEventSource) : "other",
    at: e.at,
    expiresAt: e.expiresAt ?? null,
  }));
}

/** The correspondents' rows split in two: keys held now, and keys removed
 * (History). A key without `removedAt` (an older server) is held. */
export function splitRemoved<T extends { key: Pick<HeldContactKey, "removedAt"> }>(rows: readonly T[]): {
  held: T[];
  removed: T[];
} {
  const held: T[] = [];
  const removed: T[] = [];
  for (const r of rows) (r.key.removedAt ? removed : held).push(r);
  return { held, removed };
}

/**
 * The address ContactKey/decide is asked about for a key (it finds the contact
 * by address, then the key by fingerprint): an address the key is the pin for,
 * else one its own user IDs name that the contact has, else the contact's
 * first, else the key's first. Null when there is none.
 */
export function decideAddress(
  key: Pick<HeldContactKey, "pinnedFor" | "addresses">,
  contactAddresses: readonly string[],
): string | null {
  const own = new Set(contactAddresses.map((a) => a.toLowerCase()));
  return (
    key.pinnedFor[0] ??
    key.addresses.find((a) => own.has(a.toLowerCase())) ??
    contactAddresses[0] ??
    key.addresses[0] ??
    null
  );
}

/**
 * The held key compose's "Can't encrypt: X's key expired" line refreshes: a
 * key for that address (named by the key or held under a contact who has it),
 * not removed; the pin first, then an expired one, then the most recently
 * fetched. Null when none is held.
 */
export function heldKeyForAddress(
  keys: readonly HeldContactKey[],
  contacts: readonly { id: Id; addresses: { address: string }[] }[],
  address: string,
): HeldContactKey | null {
  const a = address.trim().toLowerCase();
  const contactHas = new Set(
    contacts.filter((c) => c.addresses.some((x) => x.address.toLowerCase() === a)).map((c) => c.id),
  );
  const candidates = keys.filter(
    (k) => !k.removedAt && (k.addresses.some((x) => x.toLowerCase() === a) || contactHas.has(k.contactId)),
  );
  const rank = (k: HeldContactKey): number =>
    (k.pinnedFor.some((x) => x.toLowerCase() === a) ? 4 : 0) + (k.expired ? 2 : 0);
  candidates.sort((x, y) => rank(y) - rank(x) || y.fetchedAt.localeCompare(x.fetchedAt));
  return candidates[0] ?? null;
}

// ---------------------------------------------------------------------------
// Refresh (#818)
// ---------------------------------------------------------------------------

/** What a refresh found, in the closed set the page words. */
export type RefreshOutcome =
  | { kind: "renewed"; expiresAt: string | null }
  | { kind: "unchanged" }
  | { kind: "differentKey"; newFingerprint: string | null; newKeyId: Id | null }
  | { kind: "revoked" }
  | { kind: "updated" }
  | { kind: "notFound" }
  | { kind: "unreachable" }
  | { kind: "unknown" };

export function refreshOutcome(r: ContactKeyRefreshResult): RefreshOutcome {
  switch (r.result) {
    case "renewed":
      return { kind: "renewed", expiresAt: r.key?.expiresAt ?? null };
    case "unchanged":
      return { kind: "unchanged" };
    case "differentKey":
      return { kind: "differentKey", newFingerprint: r.newFingerprint ?? null, newKeyId: r.newKeyId ?? null };
    case "revoked":
      return { kind: "revoked" };
    case "updated":
      return { kind: "updated" };
    case "notFound":
      return { kind: "notFound" };
    case "unreachable":
      return { kind: "unreachable" };
    default:
      // A result this version does not know is not "unchanged" or "not found".
      return { kind: "unknown" };
  }
}

/** What compose's "Check for an updated key" ends in: what the refresh found,
 * no key held for the address after all, or why the check did not run. */
export type ComposeRefreshResult = RefreshOutcome | { kind: "noKey" } | { kind: "error"; error: KeyActionError };

/** Whether "Check for an updated key" is the prominent action on a key: it is
 * about to stop working, or has. */
export function refreshIsProminent(k: Pick<HeldContactKey, "expired" | "expiringSoon" | "revoked">): boolean {
  return !k.revoked && (k.expired || k.expiringSoon);
}

// ---------------------------------------------------------------------------
// Replace (#817)
// ---------------------------------------------------------------------------

const ULID_IN_PARENS = /\(([0-9A-HJKMNP-TV-Z]{26})\)/;

/**
 * When UserKey/replace failed AFTER the new key was made, the server's error
 * names the new key (user_key_replace.go: "A new key (<id>) was made but not
 * put in place; your current key is unchanged."). That id, or null when the
 * failure came before anything was generated. Only the error types the store
 * step can end in are read; the id is the signal, never the English around it.
 */
export function replaceLeftNewKey(err: unknown): Id | null {
  if (!(err instanceof JmapMethodError)) return null;
  if (!["serverFail", "keyRevoked", "keyRetired", "keyNotBound"].includes(err.type)) return null;
  const m = ULID_IN_PARENS.exec(err.description ?? "");
  return m ? (m[1] as Id) : null;
}
