// SPDX-License-Identifier: AGPL-3.0-only

// Settings → Keys (#796, #795): the parts of managing your own keys and your
// correspondents' keys that are not UI. The server (internal/api/
// user_key_manage.go, contact_key_get.go) is the authority for every fact and
// every refusal; this file turns its facts into what the page states and its
// refusals into a closed set the page words in plain language.
//
// ⚠ NO RAW CODES REACH THE SCREEN. Every method error and SetError type the
// server documents has its own sentence (`KEY_ERROR_TEXT` in the UI); anything
// else is `failed`, never a guess at a cause the server did not name.
//
// ⚠ AN ABSENT FACT IS NOT A FALSE ONE. A server before v1.93 sends none of the
// expiry flags, and "expiresAt absent" must read as "not said" rather than as
// "never expires" — the page prints nothing about expiry then (#176).

import { JmapMethodError, JmapTransportError } from "./jmap/client";
import type { HeldContactKey, Id, Identity, SetError, UserKey } from "./jmap/types";

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/** Every way a key action can fail, as the page words them. */
export type KeyActionError =
  // method errors (any key method)
  | "keyManagementUnavailable"
  | "notFound"
  | "rateLimited"
  | "invalidArguments"
  | "wildcardIdentity"
  | "publicKeyRefused"
  | "invalidKey"
  | "tooLarge"
  // UserKey/replace (#817), ContactKey/refresh (#818), ContactKey/decide
  | "keyRevoked"
  | "keyNotBound"
  | "wkdUnavailable"
  | "keyDoesNotNameAddress"
  // UserKey/set SetErrors
  | "oneChangeAtATime"
  | "keyIsDefault"
  | "keyIsPublished"
  | "keyRetired"
  | "wkdNotConfigured"
  | "notPublishable"
  | "retireIsPermanent"
  | "invalidProperties"
  // the client's own
  | "unavailable"
  | "failed";

const METHOD_ERRORS: readonly KeyActionError[] = [
  "keyManagementUnavailable",
  "notFound",
  "rateLimited",
  "invalidArguments",
  "wildcardIdentity",
  "publicKeyRefused",
  "invalidKey",
  "tooLarge",
  "keyRevoked",
  "keyRetired",
  "keyNotBound",
  "wkdUnavailable",
  "keyDoesNotNameAddress",
];

const SET_ERRORS: readonly KeyActionError[] = [
  "oneChangeAtATime",
  "keyIsDefault",
  "keyIsPublished",
  "keyRetired",
  "wkdNotConfigured",
  "notPublishable",
  "retireIsPermanent",
  "notFound",
  "invalidProperties",
];

/** A thrown error from a key method, mapped to the closed set. An unknown
 * method-error type is `failed`: the page must not claim a cause it was not
 * told. A transport failure is `unavailable` (the request may not have run). */
export function keyErrorOf(err: unknown): KeyActionError {
  if (err instanceof JmapMethodError) {
    return (METHOD_ERRORS as readonly string[]).includes(err.type) ? (err.type as KeyActionError) : "failed";
  }
  if (err instanceof JmapTransportError) return "unavailable";
  return "failed";
}

/** A refused UserKey/set or ContactKey/set update, mapped the same way. */
export function keySetErrorOf(e: SetError): KeyActionError {
  return (SET_ERRORS as readonly string[]).includes(e.type) ? (e.type as KeyActionError) : "failed";
}

/** What a key action resolves to: done, or why not. */
export type KeyOutcome<T = undefined> = { ok: true; value: T } | { ok: false; error: KeyActionError };

/** Run one key method and fold its failure into the closed set. */
export async function keyOutcome<T>(run: () => Promise<T>): Promise<KeyOutcome<T>> {
  try {
    return { ok: true, value: await run() };
  } catch (err) {
    return { ok: false, error: keyErrorOf(err) };
  }
}

/** Run one UserKey/set or ContactKey/set and fold its SetError or failure. */
export async function keySetOutcome(run: () => Promise<SetError | null>): Promise<KeyOutcome> {
  try {
    const refused = await run();
    return refused ? { ok: false, error: keySetErrorOf(refused) } : { ok: true, value: undefined };
  } catch (err) {
    return { ok: false, error: keyErrorOf(err) };
  }
}

// ---------------------------------------------------------------------------
// Dates and expiry
// ---------------------------------------------------------------------------

/** "2 Nov 2028" — the long form, for Settings. The raw string when this
 * browser cannot parse it (provenance is worth showing verbatim). */
export function keyDateLong(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric" });
}

/** "2 Nov" — the short form for a notice, with the year only when it is not
 * this year (a key expiring in 2028 must not read as this November). */
export function keyDateShort(iso: string, now: Date): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  if (d.getFullYear() !== now.getFullYear()) return keyDateLong(iso);
  return d.toLocaleDateString("en-GB", { day: "numeric", month: "short" });
}

/** What the page says about a key's expiry. `unknown`: the server did not say
 * (an older server), so nothing is printed. */
export type ExpiryFact =
  | { kind: "never" }
  | { kind: "expires"; at: string }
  | { kind: "expired"; at: string | null }
  | { kind: "unknown" };

export function expiryFact(key: Pick<UserKey, "expiresAt" | "expired">): ExpiryFact {
  if (key.expired === true) return { kind: "expired", at: key.expiresAt ?? null };
  if (key.expiresAt === undefined) return { kind: "unknown" };
  if (key.expiresAt === null) return key.expired === false ? { kind: "never" } : { kind: "unknown" };
  return { kind: "expires", at: key.expiresAt };
}

const DAY_MS = 24 * 60 * 60 * 1000;

/**
 * Whether a key is inside the warning window. The server's own `expiringSoon`
 * wins whenever it is present; only a server that sent a date and no flag is
 * judged here, and then against the server's own threshold (`warnDays`, from
 * the capability) — never a number this client made up. No threshold, no
 * judgement.
 */
export function isExpiringSoon(
  key: { expiringSoon?: boolean; expiresAt?: string | null; usable?: boolean },
  warnDays: number | null,
  now: Date,
): boolean {
  if (typeof key.expiringSoon === "boolean") return key.expiringSoon;
  if (warnDays === null || !key.expiresAt || key.usable === false) return false;
  const at = new Date(key.expiresAt).getTime();
  if (Number.isNaN(at)) return false;
  const left = at - now.getTime();
  return left > 0 && left < warnDays * DAY_MS;
}

// ---------------------------------------------------------------------------
// Own keys
// ---------------------------------------------------------------------------

/** A retired, revoked key or a live one. */
export function isRetired(key: Pick<UserKey, "decryptOnly" | "revokedAt">): boolean {
  return key.decryptOnly || key.revokedAt !== null;
}

/** The identities a key can be bound to or generated for: FIXED addresses
 * only. A domain wildcard has no single address to put on a key. */
export function fixedIdentities(identities: readonly Identity[]): Identity[] {
  return identities
    .filter((i) => !i.isWildcard && i.email.trim() !== "" && !i.email.startsWith("*@"))
    .slice()
    .sort((a, b) => a.email.localeCompare(b.email));
}

/** The status chips beside a key, in reading order. */
export type KeyChip =
  | { kind: "signsAs"; address: string }
  | { kind: "published" }
  | { kind: "expiringSoon" }
  | { kind: "expired" }
  | { kind: "retired" }
  | { kind: "revoked" };

export function keyChips(
  key: UserKey,
  identities: readonly Identity[],
  warnDays: number | null,
  now: Date,
): KeyChip[] {
  const chips: KeyChip[] = [];
  // "Signs as" reads Identity.signingKeyId, which the server computes from the
  // packets (ADR-040 §7): an expired key is bound but signs for nobody, and the
  // chip says only what is true.
  for (const i of identities) {
    if (i.signingKeyId === key.id) chips.push({ kind: "signsAs", address: i.email });
  }
  // A revoked key can stay published on purpose: WKD then serves the
  // revocation (ADR-040 §4), so the chip says what the directory holds.
  if (key.wkdPublished) chips.push({ kind: "published" });
  if (key.revokedAt !== null) chips.push({ kind: "revoked" });
  else if (key.decryptOnly) chips.push({ kind: "retired" });
  if (key.revokedAt === null && !key.decryptOnly) {
    if (key.expired === true) chips.push({ kind: "expired" });
    else if (isExpiringSoon(key, warnDays, now)) chips.push({ kind: "expiringSoon" });
  }
  return chips;
}

/** Why Publish is not offered for a key, or null when it is. */
export type PublishBlock = "wkdOff" | "notBound" | "notServable" | "retired" | "unknown";

export function publishBlock(key: UserKey, wkdConfigured: boolean | null): PublishBlock | null {
  if (isRetired(key)) return "retired";
  if (wkdConfigured === false) return "wkdOff";
  if (wkdConfigured === null) return "unknown";
  if (key.identityId === null) return "notBound";
  if (key.wkdServable === false) return "notServable";
  return null;
}

/** The address a key is bound to, from the store's identities. */
export function boundAddress(key: Pick<UserKey, "identityId">, identities: readonly Identity[]): string | null {
  if (key.identityId === null) return null;
  return identities.find((i) => i.id === key.identityId)?.email ?? null;
}

// ---------------------------------------------------------------------------
// The deliberate steps
// ---------------------------------------------------------------------------

/** The last 8 hex digits of a fingerprint, upper case: what revoking asks the
 * user to type, and what the server checks. */
export function fingerprintTail(fingerprint: string): string {
  return fingerprint.replace(/[^0-9A-Fa-f]/g, "").toUpperCase().slice(-8);
}

/** Whether what the user typed names this key. Spaces and case are forgiven
 * (they are copying a grouped, upper-case string); nothing else is. */
export function revokeConfirmMatches(fingerprint: string, typed: string): boolean {
  const tail = fingerprintTail(fingerprint);
  return tail.length === 8 && typed.replace(/\s+/g, "").toUpperCase() === tail;
}

/** The server's minimum (UserKey/export, MinExportPassphrase). */
export const MIN_BACKUP_PASSPHRASE = 12;

/** Why a backup passphrase cannot be used yet, or null. Counted in characters
 * (code points), as the server counts runes. */
export function backupPassphraseProblem(passphrase: string, confirm: string): "tooShort" | "mismatch" | null {
  if ([...passphrase].length < MIN_BACKUP_PASSPHRASE) return "tooShort";
  if (passphrase !== confirm) return "mismatch";
  return null;
}

/** The server's cap on a key's lifetime (MaxKeyLifetimeDays). */
export const MAX_KEY_DAYS = 3650;

/** Days for an extension: 1 or 2 years, or a custom whole number of days in
 * 1..3650; null when the custom value is not one. */
export function extendDays(choice: "1y" | "2y" | "custom", custom: string): number | null {
  if (choice === "1y") return 365;
  if (choice === "2y") return 730;
  const t = custom.trim();
  if (!/^\d+$/.test(t)) return null;
  const n = Number(t);
  return n >= 1 && n <= MAX_KEY_DAYS ? n : null;
}

/** Days for a new key's expiry; undefined is the server's default (2 years). */
export function generateExpiryDays(choice: "1y" | "2y" | "5y" | "never"): number | undefined {
  switch (choice) {
    case "1y":
      return 365;
    case "2y":
      return undefined;
    case "5y":
      return 5 * 365;
    case "never":
      return 0;
  }
}

/**
 * Save the wrapped key as a file, and leave nothing behind: the object URL is
 * revoked straight after the click, and the caller drops the text. `doc` is
 * injectable for tests.
 */
export function downloadArmoredKey(armored: string, fileName: string, doc: Document = document): void {
  const blob = new Blob([armored], { type: "application/pgp-keys" });
  const url = URL.createObjectURL(blob);
  try {
    const a = doc.createElement("a");
    a.href = url;
    a.download = fileName;
    a.rel = "noopener";
    a.style.display = "none";
    doc.body.appendChild(a);
    a.click();
    a.remove();
  } finally {
    URL.revokeObjectURL(url);
  }
}

// ---------------------------------------------------------------------------
// Correspondents' keys
// ---------------------------------------------------------------------------

/** Where a correspondent key came from, as the page words it. */
export type ContactKeySource = "imported" | "wkd" | "autocryptAuthenticated" | "autocrypt" | "keyserver" | "other";

export function contactKeySource(k: Pick<HeldContactKey, "source" | "authenticated">): ContactKeySource {
  switch (k.source) {
    case "manual":
      return "imported";
    case "wkd":
      return "wkd";
    case "autocrypt":
      return k.authenticated ? "autocryptAuthenticated" : "autocrypt";
    case "keyserver":
      return "keyserver";
    default:
      return "other";
  }
}

/** One row of the correspondents' list: the key, who it belongs to. */
export interface CorrespondentKeyRow {
  key: HeldContactKey;
  /** The contact's name, or its first address, or the key's first address. */
  name: string;
  /** The addresses to show: the key's own, else the contact's. */
  addresses: string[];
}

export function correspondentRows(
  keys: readonly HeldContactKey[],
  contacts: readonly { id: Id; displayName: string; addresses: { address: string }[] }[],
): CorrespondentKeyRow[] {
  const byId = new Map(contacts.map((c) => [c.id, c]));
  return keys
    .map((key) => {
      const c = byId.get(key.contactId);
      const contactAddrs = c ? c.addresses.map((a) => a.address) : [];
      const addresses = key.addresses.length > 0 ? key.addresses : contactAddrs;
      const name = (c?.displayName ?? "").trim() || addresses[0] || contactAddrs[0] || key.fingerprint;
      return { key, name, addresses };
    })
    .sort((a, b) => a.name.localeCompare(b.name) || a.key.fingerprint.localeCompare(b.key.fingerprint));
}
