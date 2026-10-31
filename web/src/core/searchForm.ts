// SPDX-License-Identifier: AGPL-3.0-only

// THE SEARCH-OPTIONS FORM, AS A PURE MAPPING (#724).
//
// The form is NOT a second search path. It is an editor for the query string:
// opening it reads the current query into fields, and pressing Search writes
// the fields back into a query string, which then runs exactly as if it had
// been typed. So there is one search language, one parser (the server's), and
// one thing to bookmark, share or save.
//
// The hard requirement is the direction people forget: READING A QUERY INTO THE
// FORM MUST NEVER LOSE ANY OF IT. A query the form cannot represent as fields —
// an OR, a group, a second `from:`, a date the select has no option for, an
// operator the form has no field for — keeps that part, verbatim and in order,
// in "Has the words". Nothing is dropped; the worst case is that nothing is
// lifted out.
//
// The rules that make that safe:
//   * ONLY top-level parts are lifted, and NOTHING is lifted from a query with
//     a top-level OR: OR binds looser than the implicit AND, so moving a part
//     out of `a OR b from:x` would change which side `from:x` constrains.
//   * a part is lifted only if writing the field back yields the SAME TOKEN
//     (bar the operator's letter case). `subject:"invoice"` is a phrase search
//     and `subject:invoice` is not, so the first stays in the words rather than
//     being silently turned into the second.
//   * each field holds one value; a second `from:` stays in the words.
//   * a negated part of any kind (`-word`, `-"a phrase"`, `-from:x`, `-(a b)`)
//     goes to "Doesn't have", and comes back out with its `-` restored.
// And on the way out, "Has the words" is bracketed when it holds a top-level
// OR and anything else is being written beside it, for the same reason.

import { hasTopLevelOr, operatorTerm, segments } from "./searchQuery";
import type { Segment } from "./searchQuery";

/** The "Date within" choices: values of `newer_than:` (#722). A query with a
 * value outside this list keeps it in the words rather than losing it. */
export const WITHIN_VALUES = ["1d", "3d", "1w", "2w", "1m", "2m", "6m", "1y"] as const;
export type Within = (typeof WITHIN_VALUES)[number];

/** "Has attachment / type": `has:attachment` (any) or one of the attachment
 * families internal/search/attachtype.go defines — the same values
 * searchSuggest.ts HAS_VALUES offers. */
export const ATTACHMENT_VALUES = ["any", "pdf", "image", "document", "spreadsheet", "presentation"] as const;
export type Attachment = (typeof ATTACHMENT_VALUES)[number];

export interface SearchFormFields {
  from: string;
  to: string;
  subject: string;
  /** Free text — and everything the form could not lift into a field. */
  hasWords: string;
  /** Parts to exclude, each written back with a leading `-`. */
  doesntHave: string;
  within: Within | "";
  /** `larger:` / `smaller:` values as the parser reads them, e.g. `5M`. */
  larger: string;
  smaller: string;
  /** An area NAME (`area:` resolves names, #722). */
  area: string;
  attachment: Attachment | "";
  /** `in:anywhere` — Trash and Junk are left out of a search unless asked. */
  includeTrashJunk: boolean;
}

export const EMPTY_FORM: SearchFormFields = {
  from: "",
  to: "",
  subject: "",
  hasWords: "",
  doesntHave: "",
  within: "",
  larger: "",
  smaller: "",
  area: "",
  attachment: "",
  includeTrashJunk: false,
};

/** The text-valued fields that map one-to-one onto an operator. */
const TEXT_FIELDS = [
  ["from", "from"],
  ["to", "to"],
  ["subject", "subject"],
  ["larger", "larger"],
  ["smaller", "smaller"],
  ["area", "area"],
] as const;

function isWithin(v: string): v is Within {
  return (WITHIN_VALUES as readonly string[]).includes(v);
}

function hasValueOf(a: Attachment): string {
  return a === "any" ? "attachment" : a;
}

function attachmentOf(v: string): Attachment | null {
  if (v === "attachment") return "any";
  return (ATTACHMENT_VALUES as readonly string[]).includes(v) && v !== "any" ? (v as Attachment) : null;
}

/** The part as the form would write it, compared with the operator's letter
 * case folded (the parser folds it too). */
function sameToken(seg: Segment, written: string): boolean {
  const colon = seg.raw.indexOf(":");
  const raw = colon > 0 ? seg.raw.slice(0, colon).toLowerCase() + seg.raw.slice(colon) : seg.raw;
  return raw === written;
}

/** Reads a query into the form. Never drops anything: what is not lifted into
 * a field stays in `hasWords`, in its original order. */
export function queryToForm(query: string): SearchFormFields {
  const segs = segments(query);
  const form: SearchFormFields = { ...EMPTY_FORM };
  if (segs.some((s) => s.kind === "or")) {
    return { ...form, hasWords: segs.map((s) => s.raw).join(" ") };
  }
  // A field that occurs more than once lifts NONE of its occurrences. Lifting
  // the first would write it back AFTER the second (fields are written in a
  // fixed order), and the next read would lift the other one — a query that
  // changes every time the form is opened. Both stay in the words, in order.
  const slots = new Map<string, number>();
  for (const seg of segs) {
    const slot = seg.kind === "word" && !seg.negated ? slotOf(seg) : null;
    if (slot) slots.set(slot, (slots.get(slot) ?? 0) + 1);
  }
  const words: string[] = [];
  const excluded: string[] = [];
  for (const seg of segs) {
    if (seg.negated && seg.raw.length > 1) {
      excluded.push(seg.raw.slice(1));
      continue;
    }
    const slot = seg.kind === "word" ? slotOf(seg) : null;
    if (slot && slots.get(slot) === 1 && liftInto(form, seg)) continue;
    words.push(seg.raw);
  }
  form.hasWords = words.join(" ");
  form.doesntHave = excluded.join(" ");
  return form;
}

/** Which form field a part would go to, or null for none. */
function slotOf(seg: Segment): string | null {
  switch (seg.field) {
    case "from":
    case "to":
    case "subject":
    case "larger":
    case "smaller":
    case "area":
    case "newer_than":
      return seg.field;
    case "has":
      return attachmentOf(seg.value) !== null ? "has" : null;
    case "in":
      return seg.value === "anywhere" ? "in" : null;
  }
  return null;
}

/** Puts one top-level, non-negated `field:value` into the form if a field is
 * free for it and writing it back would give the same token. */
function liftInto(form: SearchFormFields, seg: Segment): boolean {
  if (seg.value === "") return false;
  for (const [key, op] of TEXT_FIELDS) {
    if (seg.field !== op) continue;
    if (form[key] !== "" || !sameToken(seg, operatorTerm(op, seg.value))) return false;
    form[key] = seg.value;
    return true;
  }
  switch (seg.field) {
    case "newer_than":
      if (form.within !== "" || seg.quoted || !isWithin(seg.value) || !sameToken(seg, `newer_than:${seg.value}`)) {
        return false;
      }
      form.within = seg.value;
      return true;
    case "has": {
      const a = seg.quoted ? null : attachmentOf(seg.value);
      if (a === null || form.attachment !== "" || !sameToken(seg, `has:${seg.value}`)) return false;
      form.attachment = a;
      return true;
    }
    case "in":
      if (seg.value !== "anywhere" || seg.quoted || form.includeTrashJunk || !sameToken(seg, "in:anywhere")) {
        return false;
      }
      form.includeTrashJunk = true;
      return true;
  }
  return false;
}

/** Writes the form as a query string — the only thing the form produces. */
export function formToQuery(f: SearchFormFields): string {
  const parts: string[] = [];
  const text = (op: string, v: string): void => {
    if (v.trim() !== "") parts.push(operatorTerm(op, v.trim()));
  };
  text("from", f.from);
  text("to", f.to);
  text("subject", f.subject);
  const words = f.hasWords.trim();
  const others =
    f.from.trim() !== "" ||
    f.to.trim() !== "" ||
    f.subject.trim() !== "" ||
    f.doesntHave.trim() !== "" ||
    f.within !== "" ||
    f.larger.trim() !== "" ||
    f.smaller.trim() !== "" ||
    f.area.trim() !== "" ||
    f.attachment !== "" ||
    f.includeTrashJunk;
  if (words !== "") parts.push(others && hasTopLevelOr(words) ? `(${words})` : words);
  const exclude = f.doesntHave.trim();
  if (hasTopLevelOr(exclude)) {
    // "Doesn't have: a OR b" excludes the disjunction as a whole; minus-ing each
    // piece would write `-a -OR -b`, which means something else entirely.
    parts.push(`-(${exclude})`);
  } else {
    for (const seg of segments(exclude)) {
      // A part typed WITH its minus is the same exclusion, not a double negative.
      const raw = seg.negated ? seg.raw.slice(1) : seg.raw;
      if (raw !== "" && raw !== "-") parts.push(`-${raw}`);
    }
  }
  if (f.within !== "") parts.push(`newer_than:${f.within}`);
  text("larger", f.larger);
  text("smaller", f.smaller);
  text("area", f.area);
  if (f.attachment !== "") parts.push(`has:${hasValueOf(f.attachment)}`);
  if (f.includeTrashJunk) parts.push("in:anywhere");
  return parts.join(" ");
}
