// SPDX-License-Identifier: AGPL-3.0-only

// THE CONTACTS PAGE'S DECISIONS (#816, ADR-041), DOM-free so each one is
// tested without a browser (spec §12, like core/userKeys.ts for the keys page).
//
//   * which `Contact/query` each filter sends. The page lists SAVED contacts
//     (decided 2026-10-22: sending saves, VIP saves, blocking does not), so
//     "All saved" and "Has a key" send `isSaved: true`. "Blocked senders" does
//     NOT: a blocked sender is usually someone you never saved, and that list
//     replaces Settings › Senders' blocked list. "VIP" does not either, and
//     that is the server's rule followed through: un-saving a VIP KEEPS the VIP
//     marker (store.SetContactMarkers), so a VIP filter that also required
//     isSaved would hide the one VIP the user un-saved — still a VIP for
//     `is:vip`, push and quiet hours — with nowhere left to see or clear it;
//   * the name a contact goes by, and its primary address;
//   * the recent-mail search over EVERY address (`from:` and `to:`, OR-ed);
//   * what a merge combines, by ADR-041 §4's table — so the dialog says before
//     it happens that a block on either one carries over;
//   * the closed set of refusals, so no server code is ever printed.

import { hasBidiControls, stripBidiControls } from "./hostileText";
import type { Contact, ContactAddress, ContactSeparateResult, Id, SetError } from "./jmap/types";

export type ContactsFilter = "saved" | "vip" | "key" | "blocked";
export type ContactsSort = "name" | "last";

export const CONTACTS_FILTERS: readonly ContactsFilter[] = ["saved", "vip", "key", "blocked"];

/** How many contacts one "Show more" asks for. Under the server's cap of 100. */
export const CONTACTS_PAGE = 50;

/** The user's own text limits (ADR-041 §2; the server CHECKs the same). */
export const MAX_USER_NAME = 200;
export const MAX_NOTES = 4000;

export function isContactsFilter(s: string | undefined): s is ContactsFilter {
  return s !== undefined && (CONTACTS_FILTERS as readonly string[]).includes(s);
}

/** The `Contact/query` arguments for one view of the list. */
export function contactsQueryArgs(opts: {
  filter: ContactsFilter;
  sort: ContactsSort;
  text: string;
  position: number;
  limit?: number;
}): {
  filter: Record<string, unknown>;
  sort: { property: string; isAscending: boolean }[];
  position: number;
  limit: number;
} {
  const filter: Record<string, unknown> = {};
  switch (opts.filter) {
    case "saved":
      filter["isSaved"] = true;
      break;
    case "key":
      filter["isSaved"] = true;
      filter["hasKey"] = true;
      break;
    case "vip":
      // Not isSaved: see the header (un-saving keeps VIP).
      filter["isVip"] = true;
      break;
    case "blocked":
      // Not isSaved: blocked senders are usually not saved.
      filter["isBlocked"] = true;
      break;
  }
  const text = opts.text.trim();
  if (text !== "") filter["text"] = text;
  return {
    filter,
    // Latest first for "Last mail": RFC 8620's default is ascending, so the
    // direction is always sent rather than left to the default.
    sort:
      opts.sort === "last"
        ? [{ property: "lastMailAt", isAscending: false }]
        : [{ property: "name", isAscending: true }],
    position: opts.position,
    limit: opts.limit ?? CONTACTS_PAGE,
  };
}

// -- names, as a stranger may have written them ---------------------------------
//
// EVERY STRING HERE CAN BE THE SENDER'S. `displayName` is learned from their own
// From header, so `name` is theirs too until the user names them, and so are
// the addresses and `reachedMeAt`. A directional override (U+202E and friends)
// can reorder what the reader sees, and a display name that is itself an
// address ("ceo@example.com") can pass for another contact. So every contacts
// surface reads a contact's text through THIS one formatter, which is the
// reading pane's rule (core/hostileText.ts senderIdentity) applied to a
// contact: bidi controls stripped (plus the three directional marks the server
// strips from a userName), and the real address shown beside a name that
// contained an address-like "@" or had controls in it. The user's own userName
// is cleaned by the server already and goes through the same path anyway.

const DIRECTIONAL_MARKS = /[\u200E\u200F\u061C]/;
const DIRECTIONAL_MARKS_ALL = /[\u200E\u200F\u061C]/g;

/** A contact string as it may be shown: no bidi controls, no directional
 * marks, trimmed. */
export function cleanContactText(s: string | null | undefined): string {
  return stripBidiControls(s ?? "").replace(DIRECTIONAL_MARKS_ALL, "").trim();
}

function hadControls(s: string | null | undefined): boolean {
  return hasBidiControls(s) || DIRECTIONAL_MARKS.test(s ?? "");
}

export interface ContactIdentity {
  /** What the contact goes by, cleaned: its name, else its primary address. */
  name: string;
  /** The primary address, cleaned. */
  address: string;
  /** Show the address beside the name: the name could pass for another
   * contact (it contains "@", or it carried directional controls). */
  showAddress: boolean;
}

/** THE formatter (see above). */
export function contactIdentity(c: Contact): ContactIdentity {
  const raw = c.name ?? c.displayName ?? "";
  const name = cleanContactText(raw);
  const rawAddress = c.addresses[0]?.address ?? "";
  const address = cleanContactText(rawAddress);
  const confusable = name.includes("@") || hadControls(raw);
  return {
    name: name !== "" ? name : address,
    address,
    showAddress: name !== "" && address !== "" && name.toLowerCase() !== address.toLowerCase() && confusable,
  };
}

/** The first address, cleaned, which the list and the page call primary. */
export function primaryAddress(c: Contact): string {
  return contactIdentity(c).address;
}

/** The name a contact goes by, cleaned: the server's `name` (userName, else the
 * learned displayName), else the primary address. A server before #816 sends
 * no `name`; its displayName stands in. */
export function contactName(c: Contact): string {
  return contactIdentity(c).name;
}

/** The one-line form, for a sentence, a title or an aria-label: the name, with
 * the address in angle brackets when the name alone could mislead. */
export function contactLabel(c: Contact): string {
  const id = contactIdentity(c);
  return id.showAddress ? `${id.name} <${id.address}>` : id.name;
}

/** The learned name to show beside the user's own ("their mail says …"),
 * cleaned, or null when there is no user name, no learned one, or they are the
 * same. */
export function learnedNameBeside(c: Contact): string | null {
  const own = cleanContactText(c.userName);
  const learned = cleanContactText(c.displayName);
  if (own === "" || learned === "" || own === learned) return null;
  return learned;
}

/** Whether this server speaks the saved-contact surface at all (#816). A
 * contact from an older server carries no `isSaved`. */
export function hasSavedSurface(c: Contact): boolean {
  return typeof c.isSaved === "boolean";
}

/** Characters as the server counts them (runes, after CRLF becomes LF), so the
 * live count and the limit agree with the refusal. */
export function notesLength(s: string): number {
  return [...s.replace(/\r\n?/g, "\n")].length;
}

export function userNameLength(s: string): number {
  return [...s.trim()].length;
}

/** "N of M", the bound of a page (CLAUDE.md: a bounded result reports its bound). */
export function pageBound(shown: number, total: number): { shown: number; total: number; complete: boolean } {
  return { shown, total, complete: shown >= total };
}

// -- recent mail ----------------------------------------------------------------

/** An address the search syntax carries unquoted, and means only that address:
 * a domain-shaped value would be read as the whole domain (#712), and a space,
 * quote or bracket would change the query's structure. */
export function searchableAddress(a: string): boolean {
  return /^[^\s"()@]+@[^\s"()@]+\.[^\s"()@]+$/.test(a) && !a.startsWith("*") && !a.startsWith("-");
}

/**
 * The search for mail with this person: `from:` and `to:` for EVERY address,
 * OR-ed (internal/search/parse.go: OR is upper case only; `to:` is To or Cc).
 * An address the syntax cannot carry is left out AND REPORTED, so the page
 * can say the list does not cover it rather than implying it does.
 */
export function recentMailQuery(addresses: readonly ContactAddress[]): { query: string; skipped: string[] } {
  const terms: string[] = [];
  const skipped: string[] = [];
  for (const { address } of addresses) {
    if (!searchableAddress(address)) {
      skipped.push(address);
      continue;
    }
    terms.push(`from:${address}`, `to:${address}`);
  }
  return { query: terms.join(" OR "), skipped };
}

// -- merge ----------------------------------------------------------------------

export interface MergePreview {
  /** The name the merged contact will go by. */
  name: string;
  /** Every address it will have, into's first. */
  addresses: string[];
  /** How many keys move with `from`. */
  keysFrom: number;
  vip: boolean;
  saved: boolean;
  /** Null when neither is blocked. Otherwise where new mail goes, and whose
   * block it is, so the dialog can say a block carries over. */
  block: { destination: "junk" | "trash"; from: "into" | "from" | "both" } | null;
  /** Both have notes: they are joined, into's first. */
  notesJoined: boolean;
  /** The joined notes would pass the limit: the server refuses (notesTooLong). */
  notesTooLong: boolean;
}

/** ADR-041 §4's combine rules, as the dialog states them before the merge. */
export function mergePreview(into: Contact, from: Contact): MergePreview {
  const intoNotes = (into.notes ?? "").trim();
  const fromNotes = (from.notes ?? "").trim();
  const notesJoined = intoNotes !== "" && fromNotes !== "";
  const joined = notesJoined ? `${intoNotes}\n---\n${fromNotes}` : intoNotes || fromNotes;
  const ib = into.isBlocked === true;
  const fb = from.isBlocked === true;
  let block: MergePreview["block"] = null;
  if (ib || fb) {
    // The stricter destination among the BLOCKED sides (trash > junk).
    const dests = [ib ? into.blockDestination : null, fb ? from.blockDestination : null];
    const destination = dests.includes("trash") ? "trash" : "junk";
    block = { destination, from: ib && fb ? "both" : ib ? "into" : "from" };
  }
  const userName = cleanContactText(into.userName) || cleanContactText(from.userName);
  const learned = cleanContactText(into.displayName) || cleanContactText(from.displayName);
  const addresses = [...into.addresses, ...from.addresses].map((a) => cleanContactText(a.address));
  return {
    name: userName || learned || addresses[0] || "",
    addresses,
    keysFrom: from.keys?.length ?? 0,
    vip: into.isVip === true || from.isVip === true,
    saved: into.isSaved === true || from.isSaved === true,
    block,
    notesJoined,
    notesTooLong: notesLength(joined) > MAX_NOTES,
  };
}

/** Separate: which address ids may be split off — at least one, never all. */
export function separateAllowed(c: Contact, chosen: ReadonlySet<Id>): "ok" | "none" | "all" {
  if (chosen.size === 0) return "none";
  const ids = c.addresses.map((a) => a.id).filter((x): x is Id => typeof x === "string");
  if (ids.every((id) => chosen.has(id))) return "all";
  return "ok";
}

/** The fields a separate can report as kept that the user can edit, in the
 * page's order. `displayName` (the name from their mail) is left out: only
 * mail changes it, so "your edits" would be untrue. */
export const KEPT_FIELDS = ["userName", "notes", "isVip", "isSaved", "isBlocked"] as const;
export type KeptField = (typeof KEPT_FIELDS)[number];

/** What a separate did to the contact it left (#834, ADR-041 §4): its merge
 * was undone (with the user's later edits kept), or nothing on it changed
 * back. Null from a server that does not say. */
export type SeparateOutcome = { kind: "restored"; kept: KeptField[] } | { kind: "unchanged" } | null;

export function separateOutcome(r: ContactSeparateResult): SeparateOutcome {
  if (r.restored === undefined) return null;
  if (!r.restored) return { kind: "unchanged" };
  const kept = new Set(r.keptFields ?? []);
  return { kind: "restored", kept: KEPT_FIELDS.filter((f) => kept.has(f)) };
}

// -- refusals -------------------------------------------------------------------

/** Every way a contacts write or read can fail, said in words by the page. */
export type ContactError =
  | "notFound"
  | "userNameTooLong"
  | "notesTooLong"
  | "vipConflict"
  | "invalid"
  | "stateMismatch"
  | "unavailable"
  | "failed";

/** A SetError from Contact/set, named by the property the server refused. */
export function contactSetErrorOf(e: SetError): ContactError {
  if (e.type === "notFound") return "notFound";
  if (e.type === "invalidProperties") {
    const props = e.properties ?? [];
    if (props.includes("isVip") && props.includes("isSaved")) return "vipConflict";
    if (props.includes("userName")) return "userNameTooLong";
    if (props.includes("notes")) return "notesTooLong";
    return "invalid";
  }
  return "failed";
}

/** A method error (Contact/merge, Contact/separate, a query) or a transport one. */
export function contactMethodErrorOf(err: unknown): ContactError {
  const type =
    err && typeof err === "object" && "type" in err && typeof (err as { type: unknown }).type === "string"
      ? (err as { type: string }).type
      : null;
  switch (type) {
    case "notFound":
      return "notFound";
    case "notesTooLong":
      return "notesTooLong";
    case "stateMismatch":
      return "stateMismatch";
    case "invalidArguments":
      return "invalid";
    case "unknownMethod":
    case "unsupportedFilter":
    case "unsupportedSort":
      return "unavailable";
    default:
      return "failed";
  }
}
