// SPDX-License-Identifier: AGPL-3.0-only

// THE QUERY STRING, READ THE WAY THE SERVER READS IT (#724).
//
// Two things in the search box need to take a query apart: the suggestion list
// (which token is the caret in, and which operator does it belong to?) and the
// search-options form (which parts of this query can be shown as fields?). Both
// must split the string exactly where internal/search/parse.go's tokenizer does,
// or a completion lands in the wrong place and a form rewrites a query into a
// different one. This module is that split, kept DOM-free and pure.
//
// What it mirrors from the Go tokenizer, and nothing more:
//   * whitespace separates tokens, except inside double quotes (there is no
//     escape character — a `"` always toggles quoting);
//   * a token that starts with `(` (or `-(`) opens a bracketed group, which
//     runs to its matching `)`;
//   * a leading `-` negates the token or group it is attached to;
//   * `name:` is an operator only when `name` is one of OPERATOR_NAMES
//     (case-insensitive) — `https://x` stays plain text, as on the server;
//   * a bare upper-case `OR` is the disjunction.
//
// It does NOT evaluate anything. A group is kept as its raw text; the form
// never looks inside one, and the suggester only looks at the token the caret
// is in.

/** Every operator the parser knows — internal/search/parse.go `operatorNames`.
 * internal/search/suggest_vocab_test.go reads this array and fails if it and
 * the Go list ever differ IN EITHER DIRECTION: an operator missing here would
 * make the form treat `name:value` as plain words, and one extra would make it
 * lift text the server reads as words into a field. */
export const OPERATOR_NAMES = [
  "from",
  "to",
  "cc",
  "recipient",
  "alias",
  "deliveredto",
  "subject",
  "body",
  "filename",
  "label",
  "account",
  "area",
  "list",
  "is",
  "has",
  "in",
  "keyword",
  "after",
  "since",
  "before",
  "until",
  "on",
  "date",
  "newer_than",
  "older_than",
  "larger",
  "smaller",
];

const OPERATORS = new Set(OPERATOR_NAMES);

export function isOperatorName(name: string): boolean {
  return OPERATORS.has(name.toLowerCase());
}

/** One top-level piece of a query: a word (possibly `field:value`), a whole
 * bracketed group, or the `OR` keyword. `raw` is the exact source text, so a
 * piece that is not understood can always be put back exactly as typed. */
export interface Segment {
  kind: "word" | "group" | "or";
  raw: string;
  start: number;
  end: number;
  negated: boolean;
  /** The operator, lower-cased, or "" for a bare word, a group or OR. */
  field: string;
  /** The value with its surrounding quotes removed (a word only). */
  value: string;
  /** The value was written in double quotes. */
  quoted: boolean;
}

function isSpace(c: string | undefined): boolean {
  return c === " " || c === "\t" || c === "\n" || c === "\r";
}

/** Splits a query into its top-level segments, in order. Total: never throws,
 * and the segments' raw texts, in order, are every non-whitespace character of
 * the input. */
export function segments(q: string): Segment[] {
  const out: Segment[] = [];
  const n = q.length;
  let i = 0;
  while (i < n) {
    while (i < n && isSpace(q[i])) i++;
    if (i >= n) break;
    const start = i;
    let j = i;
    const neg = q[j] === "-";
    if (neg) j++;
    if (q[j] === "(") {
      // A group: to the matching close bracket, quote-aware. An unclosed group
      // runs to the end, as the server's forgiving parser treats it.
      let depth = 0;
      let inQ = false;
      for (; j < n; j++) {
        const c = q[j];
        if (c === '"') inQ = !inQ;
        else if (!inQ && c === "(") depth++;
        else if (!inQ && c === ")") {
          depth--;
          if (depth === 0) {
            j++;
            break;
          }
        }
      }
      out.push({ kind: "group", raw: q.slice(start, j), start, end: j, negated: neg, field: "", value: "", quoted: false });
      i = j;
      continue;
    }
    let inQ = false;
    j = start;
    while (j < n) {
      const c = q[j];
      if (c === '"') inQ = !inQ;
      else if (!inQ && isSpace(c)) break;
      j++;
    }
    out.push(parseWord(q.slice(start, j), start, j));
    i = j;
  }
  return out;
}

function parseWord(raw: string, start: number, end: number): Segment {
  let body = raw;
  let negated = false;
  if (body.length > 1 && body[0] === "-") {
    negated = true;
    body = body.slice(1);
  }
  if (!negated && body === "OR") {
    return { kind: "or", raw, start, end, negated: false, field: "", value: "", quoted: false };
  }
  let field = "";
  let valueRaw = body;
  const colon = body.indexOf(":");
  if (colon > 0 && body[0] !== '"' && isOperatorName(body.slice(0, colon))) {
    field = body.slice(0, colon).toLowerCase();
    valueRaw = body.slice(colon + 1);
  }
  const quoted = valueRaw.length >= 2 && valueRaw.startsWith('"') && valueRaw.endsWith('"');
  const value = quoted ? valueRaw.slice(1, -1) : valueRaw;
  return { kind: "word", raw, start, end, negated, field, value, quoted };
}

/** True when the query has an `OR` at its top level — the one shape in which
 * moving a part of it changes what it means (`a OR b from:x` is
 * `a OR (b AND from:x)`). */
export function hasTopLevelOr(q: string): boolean {
  return segments(q).some((s) => s.kind === "or");
}

/** Writes a value the parser reads back as exactly that value: quoted when it
 * holds whitespace or a bracket, bare otherwise. A double quote cannot be
 * written inside a value at all (the tokenizer has no escape), so it is
 * dropped rather than allowed to split the query somewhere else. */
export function quoteValue(v: string): string {
  const clean = v.replace(/"/g, "");
  return /[\s()]/.test(clean) || clean === "" ? `"${clean}"` : clean;
}

/** `field:value`, written so the parser reads it back as that field and value. */
export function operatorTerm(field: string, value: string): string {
  return `${field}:${quoteValue(value)}`;
}

/**
 * The token the caret is in, for completion: the run of non-whitespace around
 * `caret`, plus the part of it that is decoration rather than the term itself —
 * a negation and/or opening brackets (`-(from:al` completes `from:al`).
 */
export interface CaretToken {
  /** Where the whole token starts and ends in the input. */
  start: number;
  end: number;
  /** `-` / `(` characters before the term, kept as typed. */
  lead: string;
  /** The term being edited (what follows `lead`, up to the caret's token end). */
  text: string;
}

export function tokenAtCaret(input: string, caret: number = input.length): CaretToken {
  const at = Math.max(0, Math.min(caret, input.length));
  let start = at;
  while (start > 0 && !isSpace(input[start - 1])) start--;
  let end = at;
  while (end < input.length && !isSpace(input[end])) end++;
  const token = input.slice(start, end);
  const m = /^[-(]*/.exec(token);
  const lead = m ? m[0] : "";
  return { start, end, lead, text: token.slice(lead.length) };
}

/**
 * What a live search (#724) should run for the text in the box right now, or
 * null when there is nothing complete to run yet.
 *
 *   * nothing typed: no search (clearing the box is the clear button's job);
 *   * an operator with no value yet (`from:`), an open quote, or a lone `-`/`(`:
 *     the user is mid-term, and running what is there would show "everything"
 *     or a parse of half a phrase — both a flicker to something unrelated;
 *   * otherwise the trimmed text, with `prefix` set when the final term is a
 *     bare word still being typed (no trailing space): the server then treats
 *     it as a prefix, so "invo" already finds "invoice".
 */
export function liveQueryOf(raw: string): { query: string; prefix: boolean } | null {
  const query = raw.trim();
  if (query === "") return null;
  if (((query.match(/"/g) ?? []).length & 1) === 1) return null;
  const segs = segments(query);
  const last = segs[segs.length - 1];
  if (!last) return null;
  if (last.kind === "word" && last.field !== "" && last.value === "") return null;
  if (/^[-(]+$/.test(last.raw)) return null;
  const prefix =
    !isSpace(raw[raw.length - 1]) && last.kind === "word" && last.field === "" && !last.quoted && !last.negated;
  return { query, prefix };
}
