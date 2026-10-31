// SPDX-License-Identifier: AGPL-3.0-only

// RECIPIENT CHIPS (#448, design pass-2 §7): the parsing and the one key fact a
// chip carries, split out of the JSX so both are testable without a renderer.
//
// WHY CHIPS AT ALL. The old To/Cc fields were plain comma-separated text, which
// means a recipient is a substring: you cannot say anything ABOUT one of them
// (that a key is held, that the address is malformed) because there is no object
// to say it about. The design needs exactly that — `⚿ key held` per recipient —
// so a completed address becomes a chip.
//
// THE KEY FACT IS THREE-VALUED, AND THAT IS THE POINT (CLAUDE.md, #176):
//
//   held    — we hold at least one OpenPGP key for this address.
//   none    — the server ANSWERED and holds none.
//   unknown — nothing has answered: the lookup has not run, failed, or this
//             server has no keys API at all. The chip prints NOTHING here,
//             because "no key" would be a claim nobody made.
//
// The distinction rides on `Contact.keys` being OPTIONAL on the wire: the server
// serves it only when it can (internal/api/contact.go contactObject), so
// `undefined` means "cannot answer" while `[]` means "answered: none". Collapsing
// those two is the mistake this whole comment exists to prevent.
//
// A MISS IS AN ANSWER. When the lookup runs and the server returns no contact for
// an address, that is the server saying it holds no key for it — `none`, not
// `unknown`. That is the spec's rule ("a miss = 'no key' only if the server
// answered") and it is what makes the fact useful: most first-time recipients
// have no Contact row at all.

import type { Contact, RecipientKeyInfo } from "../../../core/jmap/types";

export type ChipKeyFact = "held" | "none" | "unknown" | "expired" | "revoked";

/** One recipient. `name` is the display name when one is known, else null — the
 * chip prints the name and keeps the address as its title/value. */
export interface Chip {
  address: string;
  name: string | null;
}

/**
 * The key fact for one chip.
 *
 * `undefined` = not looked up (or the lookup could not be made) → unknown.
 * `null`      = looked up, and the server returned no contact → none.
 * a Contact   = `keys` decides, and its ABSENCE is `unknown` (see the header).
 */
export function chipKeyFact(contact: Contact | null | undefined): ChipKeyFact {
  if (contact === undefined) return "unknown";
  if (contact === null) return "none";
  if (contact.keys === undefined) return "unknown";
  return contact.keys.length > 0 ? "held" : "none";
}

/**
 * The chip's fact once RecipientKey/lookup has answered for the address (#809).
 *
 * `Contact.keys` counts every held key, including a lapsed one, so on its own it
 * says "key held" beside an Encrypt line that says the key expired. The lookup is
 * the server's judgement of usability (the same one the Encrypt line is built
 * from), so it wins when it is there:
 *
 *   usable                → held
 *   reason expired/revoked → expired / revoked (the key IS held, just no use)
 *   reason noKey          → none: the server answered, and there is no usable key
 *                           (even if the Contact lists one that names no address)
 *   anything else, or no lookup yet → `fallback`, the Contact-derived fact, so a
 *                           chip never flashes a state the lookup has not given.
 */
export function chipKeyFactFromLookup(
  fallback: ChipKeyFact,
  info: RecipientKeyInfo | undefined,
): ChipKeyFact {
  if (info === undefined) return fallback;
  if (info.usable) return "held";
  if (info.reason === "expired") return "expired";
  if (info.reason === "revoked") return "revoked";
  if (info.reason === "noKey") return "none";
  return fallback;
}

/** What a chip is called in a sentence: the display name if there is one, else
 * the address. One place, so the chips and the send line name people the same
 * way. */
export function chipLabel(chip: Chip): string {
  return chip.name && chip.name.trim() !== "" ? chip.name.trim() : chip.address;
}

/** A rough well-formedness check, used only to decide whether typed text is
 * READY to become a chip. Deliberately permissive: the server validates
 * recipients for real (internal/compose), and refusing to chip an address this
 * regex dislikes would stop someone sending mail to a perfectly valid one. */
export function looksLikeAddress(s: string): boolean {
  const v = s.trim();
  if (v === "" || /\s/.test(v)) return false;
  const at = v.indexOf("@");
  return at > 0 && at === v.lastIndexOf("@") && at < v.length - 1;
}

/** `Name <a@b>` → `{name: "Name", address: "a@b"}`; a bare address → no name.
 * Angle brackets and surrounding quotes are stripped; nothing else is. */
export function parseChip(raw: string): Chip | null {
  const s = raw.trim().replace(/,\s*$/, "").trim();
  if (s === "") return null;
  const m = /^(.*?)\s*<([^>]+)>$/.exec(s);
  if (m && m[2]) {
    const name = (m[1] ?? "").trim().replace(/^"(.*)"$/, "$1").trim();
    return { address: m[2].trim(), name: name === "" ? null : name };
  }
  return { address: s.replace(/^<|>$/g, ""), name: null };
}

/**
 * Splits pasted text into chips plus whatever tail is still being typed.
 *
 * SEPARATORS ARE COMMA, SEMICOLON AND NEWLINE — not spaces, because a display
 * name contains them (`Ada Lovelace <ada@example.com>`) and splitting on
 * whitespace would turn one recipient into three broken ones. A paste of
 * whitespace-separated bare addresses is still handled: a fragment with no
 * separators that contains whitespace AND more than one `@` is split on
 * whitespace as a last resort, which is what "paste of several addresses becomes
 * several chips" needs for the plain `a@b c@d` case.
 *
 * `rest` is the trailing fragment when the input does NOT end in a separator: it
 * is what the user is still typing, and turning it into a chip on every
 * keystroke would chip `a`, then `a@`, then `a@e`. When the input ends in a
 * separator the tail is empty and everything before it is a chip.
 */
export function parseChips(input: string): { chips: Chip[]; rest: string } {
  const parts = input.split(/[,;\n]/);
  const endsWithSeparator = /[,;\n]\s*$/.test(input);
  const tail = endsWithSeparator ? "" : (parts.pop() ?? "");
  const chips: Chip[] = [];
  for (const part of parts) {
    for (const frag of splitBareAddresses(part)) {
      const chip = parseChip(frag);
      if (chip && chip.address !== "") chips.push(chip);
    }
  }
  return { chips, rest: tail.trim() === "" ? "" : tail.replace(/^\s+/, "") };
}

/** The last-resort split described in parseChips: `a@x b@y` → two fragments.
 * Applied only when there is no `<…>` (so no display name can be broken) and
 * more than one `@`. */
function splitBareAddresses(part: string): string[] {
  if (part.includes("<") || part.includes(">")) return [part];
  const trimmed = part.trim();
  if (!/\s/.test(trimmed)) return [part];
  if ((trimmed.match(/@/g) ?? []).length < 2) return [part];
  return trimmed.split(/\s+/);
}

/**
 * Adds chips to a list without duplicating an address already in it.
 *
 * The de-duplication is on the LOWER-CASED address, and it keeps the FIRST
 * occurrence — so pasting a list that repeats someone does not send them two
 * copies, and the name they were first added with is the one that stays.
 */
export function addChips(existing: readonly Chip[], added: readonly Chip[]): Chip[] {
  const out = [...existing];
  const seen = new Set(out.map((c) => c.address.toLowerCase()));
  for (const chip of added) {
    const key = chip.address.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(chip);
  }
  return out;
}
