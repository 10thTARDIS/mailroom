// SPDX-License-Identifier: AGPL-3.0-only

// SEARCH SUGGESTIONS (#57, matrix §7) — completing the operator syntax as you type.
//
// Mailroom's search already understands a real operator syntax (`from:`, `label:`,
// `is:unread`, `before:`, and the rest — see internal/api/search.go's advertised
// `operators`). The problem this solves is that a syntax nobody can see is a syntax
// nobody uses: `title="…"` on the input is where help goes to die.
//
// SO SUGGESTIONS ARE THE DOCUMENTATION. Typing `l` offers `label:`; typing `label:` lists
// the labels that exist; typing `from:a` lists the people you have mail from whose
// address or name starts with `a`. The operator set is discovered by using it.
//
// EVERYTHING IT SUGGESTS, IT ALREADY KNOWS. Labels are reference data the app syncs
// anyway, and the addresses come from the `from` of the messages already loaded (which is
// in EMAIL_LIST_PROPERTIES, so it costs nothing extra). So there is NO request per
// keystroke and no server work at all — which also means suggestions keep working
// offline, where a network-backed autocomplete would just spin.
//
// The honest consequence, worth stating rather than hiding: address suggestions cover the
// people in the mail you have LOADED, not everyone you have ever corresponded with. That
// is a smaller set than the contacts table, and it is the right trade here — a search box
// that stalls on the network while you type is worse than one that completes the sender
// you were just looking at. `from:someone-not-loaded` is still a perfectly good search;
// suggestions are a shortcut, never a filter on what can be asked.
//
// IT COMPLETES THE TOKEN THE CARET IS IN, never the whole query (#724: it used to be the
// last token only, which offered nothing when the caret went back to fix an earlier
// `from:`). A search is several terms and the user is editing one of them; replacing the
// lot would throw away what they typed. The token's bounds come from searchQuery.ts,
// which splits where the server's tokenizer does.
//
// ADDRESS VALUES ALSO COME FROM THE CONTACTS STORE (#724). The loaded senders above are
// instant and work offline; the SearchBox adds the auto-derived contacts it fetched for
// the typed text (Contact/query), so `from:` can complete someone whose mail is not on
// screen. The two are merged here, loaded senders first, one row per address.
//
// Recent searches come from local history (see web/src/app/searchHistory.ts) and are
// offered only for an EMPTY input — a half-typed query and a whole remembered one are
// answers to different questions, and mixing them makes the list unpredictable.

import { quoteValue, tokenAtCaret } from "./searchQuery";

/** What kind of thing a suggestion is, so the UI can label and order it. */
export type SuggestionKind = "history" | "operator" | "value";

export interface Suggestion {
  kind: SuggestionKind;
  /** The full query text to put in the box when this is chosen. */
  apply: string;
  /** Where the caret goes in `apply`: just after the completed token. */
  caret: number;
  /** What to show. For a value suggestion this is the value alone, not the whole query. */
  label: string;
  /** Optional second line: what the operator means, or a contact's name. */
  detail?: string;
}

/** The operators the server advertises, with a one-line meaning.
 *
 * The list is duplicated from the session's `operators` deliberately: the server names
 * them, but only a human can say what each is FOR, and a suggestion with no explanation
 * teaches nothing. If the server ever advertises one that is missing here it simply is
 * not suggested — which is the safe direction, since suggesting an operator the server
 * does not implement would produce a query that silently matches nothing. */
export const SEARCH_OPERATORS: ReadonlyArray<{ op: string; detail: string }> = [
  { op: "from:", detail: "who sent it" },
  { op: "to:", detail: "who it was sent to" },
  { op: "cc:", detail: "who was copied" },
  { op: "alias:", detail: "the address it was delivered to" },
  { op: "deliveredto:", detail: "the address it was delivered to" },
  { op: "subject:", detail: "words in the subject" },
  { op: "body:", detail: "words in the message" },
  { op: "label:", detail: "one of your labels" },
  { op: "area:", detail: "an area of your desk" },
  { op: "list:", detail: "a mailing list" },
  { op: "is:", detail: "unread, flagged, snoozed, important…" },
  { op: "has:", detail: "attachment, pdf, image, unsubscribe…" },
  { op: "in:", detail: "trash, junk, or anywhere" },
  { op: "filename:", detail: "an attachment's name or extension" },
  { op: "larger:", detail: "bigger than a size, e.g. 5M" },
  { op: "smaller:", detail: "smaller than a size, e.g. 100K" },
  { op: "account:", detail: "which account" },
  { op: "keyword:", detail: "an IMAP keyword" },
  { op: "before:", detail: "older than a date" },
  { op: "after:", detail: "newer than a date" },
  { op: "on:", detail: "on one date" },
  { op: "date:", detail: "a day, month or year" },
  { op: "newer_than:", detail: "within the last 7d, 2w, 6m, 1y" },
  { op: "older_than:", detail: "older than 7d, 2w, 6m, 1y" },
];

/**
 * The values `is:` takes. Fixed, because they are states the server implements
 * — specifically, states `internal/search/parse.go`'s `flagAtom` accepts. It
 * used to also offer `unflagged`, `snoozed` and `pinned`, none of which
 * `flagAtom` recognises (#623): typing one of those three, or choosing it from
 * this very suggestion list, produced a query the parser falls back to `None{}`
 * for — a suggestion that always searched for nothing. suggest_vocab_test.go
 * (internal/search) reads this array and asserts every value it lists is one
 * `flagAtom` actually accepts, so the two vocabularies cannot drift apart again
 * without a red test on the Go side.
 *
 * `trashed` joined in v1.76.0, once a search for it returned something (#623):
 * until then Email/query removed every trashed match again.
 */
export const IS_VALUES = [
  "unread",
  "read",
  "flagged",
  "trashed",
  "snoozed",
  "muted",
  "pinned",
  "important",
  "vip",
];

/** The values `has:` takes. The attachment types are the families
 * `internal/search/attachtype.go` defines, the one place they are defined. */
export const HAS_VALUES = [
  "attachment",
  "pdf",
  "image",
  "document",
  "spreadsheet",
  "presentation",
  "unsubscribe",
];

/** The values `in:` takes (#723): search leaves trash and junk out unless asked. */
export const IN_VALUES = ["anywhere", "trash", "junk"];

/** What the suggester can draw on. All of it is already in the client store. */
export interface SuggestSources {
  labels: ReadonlyArray<{ name: string }>;
  /** Addresses seen in the loaded messages, with an optional display name. */
  contacts: ReadonlyArray<{ email: string; name?: string | null }>;
  /** Account names, when the client has them. Empty is fine: `account:` then gets no
   * value suggestions, which is better than a wrong list. */
  accounts: ReadonlyArray<{ name: string }>;
  /** The desk's area names (#724), for `area:` — which resolves an area by name. */
  areas?: ReadonlyArray<{ name: string }>;
  /** Contacts fetched for the address being typed (Contact/query), merged AFTER the
   * loaded senders: the ones on screen are the likelier intent and cost nothing. */
  directory?: ReadonlyArray<{ email: string; name?: string | null }>;
  /** Most-recent-first list of past queries. Offered only for an empty input. */
  history: ReadonlyArray<string>;
}

/**
 * The distinct senders of a set of loaded messages, most-recent-first and de-duplicated
 * by address — the address source for `from:`/`to:`/`cc:` completion.
 *
 * De-duplicated case-insensitively, keeping the FIRST display name seen for an address.
 * A sender who changes how they spell their own name would otherwise appear twice, and
 * two rows for one person pushes a second person off the list.
 */
export function sendersOf(
  emails: ReadonlyArray<{ from?: ReadonlyArray<{ email: string; name?: string | null }> | null }>,
): Array<{ email: string; name?: string | null }> {
  const seen = new Set<string>();
  const out: Array<{ email: string; name?: string | null }> = [];
  for (const e of emails) {
    for (const a of e.from ?? []) {
      const addr = a.email?.trim();
      if (!addr) continue;
      const fold = addr.toLowerCase();
      if (seen.has(fold)) continue;
      seen.add(fold);
      out.push({ email: addr, ...(a.name ? { name: a.name } : {}) });
    }
  }
  return out;
}

/** How many suggestions to return. A list longer than this is a list nobody reads. */
export const MAX_SUGGESTIONS = 8;

/** The token being edited — the text after the last space — and where it starts.
 *
 * Splitting on spaces means a quoted phrase (`subject:"end of year"`) is treated as
 * several tokens, so no value inside quotes is completed. That is a deliberate limit
 * rather than a bug: completing inside a phrase would need a real tokenizer, and getting
 * it subtly wrong would corrupt a query the user had already written correctly. */
export function lastToken(input: string): { start: number; text: string } {
  const sep = input.lastIndexOf(" ");
  return { start: sep + 1, text: input.slice(sep + 1) };
}

/** The operators whose value is an email address, completed from contacts. */
export const ADDRESS_OPERATORS: ReadonlySet<string> = new Set(["from:", "to:", "cc:", "alias:", "deliveredto:"]);

function startsWithFold(haystack: string, needle: string): boolean {
  return haystack.toLowerCase().startsWith(needle.toLowerCase());
}

function containsFold(haystack: string, needle: string): boolean {
  return haystack.toLowerCase().includes(needle.toLowerCase());
}

/**
 * Suggestions for the current input, best first.
 *
 * Never throws and never returns a suggestion whose `apply` would produce a different
 * query than the one shown: `apply` is always the input with only its last token
 * replaced.
 */
export function suggestSearch(input: string, src: SuggestSources, caret: number = input.length): Suggestion[] {
  // An empty box: offer what was searched before. Nothing else can be ranked yet, and
  // guessing would fill the list with noise at the moment the user has said least.
  if (input.trim() === "") {
    return src.history.slice(0, MAX_SUGGESTIONS).map((q) => ({
      kind: "history" as const,
      apply: q,
      caret: q.length,
      label: q,
    }));
  }

  const at = tokenAtCaret(input, caret);
  const token = at.text;
  if (token === "") return [];
  // Everything outside the token, kept exactly as typed — before AND after it.
  const replace = (term: string): { apply: string; caret: number } => {
    const head = input.slice(0, at.start) + at.lead + term;
    return { apply: head + input.slice(at.end), caret: head.length };
  };

  const colon = token.indexOf(":");
  // A completed operator (`label:`, or `from:ali`) — complete its VALUE.
  if (colon >= 0) {
    const op = token.slice(0, colon + 1).toLowerCase();
    let partial = token.slice(colon + 1);
    // A value the user opened a quote on (`area:"Side`) is still being typed.
    if (partial.startsWith('"')) partial = partial.slice(1).replace(/"$/, "");
    return valueSuggestions(op, partial, src, replace).slice(0, MAX_SUGGESTIONS);
  }

  // A bare word: offer the operators it could be the start of. The word itself is a
  // perfectly good full-text search, so it is NOT offered back as a suggestion — the
  // user can just press Enter, and echoing their own input is a wasted row.
  return SEARCH_OPERATORS.filter((o) => startsWithFold(o.op, token))
    .map((o) => ({
      kind: "operator" as const,
      ...replace(o.op),
      label: o.op,
      detail: o.detail,
    }))
    .slice(0, MAX_SUGGESTIONS);
}

function valueSuggestions(
  op: string,
  partial: string,
  src: SuggestSources,
  replace: (term: string) => { apply: string; caret: number },
): Suggestion[] {
  const value = (v: string, detail?: string): Suggestion => ({
    kind: "value",
    // Exactly the value, quoted only where the parser needs it (searchQuery.ts).
    ...replace(`${op}${quoteValue(v)}`),
    label: v,
    ...(detail ? { detail } : {}),
  });

  switch (op) {
    case "label:":
      return src.labels
        .filter((l) => partial === "" || containsFold(l.name, partial))
        .map((l) => value(l.name));
    case "is:":
      return IS_VALUES.filter((v) => startsWithFold(v, partial)).map((v) => value(v));
    case "has:":
      return HAS_VALUES.filter((v) => startsWithFold(v, partial)).map((v) => value(v));
    case "in:":
      return IN_VALUES.filter((v) => startsWithFold(v, partial)).map((v) => value(v));
    case "account:":
      return src.accounts
        .filter((a) => partial === "" || containsFold(a.name, partial))
        .map((a) => value(a.name));
    case "area:":
      return (src.areas ?? [])
        .filter((a) => partial === "" || containsFold(a.name, partial))
        .map((a) => value(a.name));
    case "from:":
    case "to:":
    case "cc:":
    case "alias:":
    case "deliveredto:":
      // Matched on BOTH address and display name, because people remember one or the
      // other and rarely the same one twice. Address-prefix matches rank first: typing
      // "ali" for alice@ is a more specific intent than matching "Natalie". What is
      // INSERTED is always the exact address, never the name.
      return rankedContacts(mergeContacts(src.contacts, src.directory ?? []), partial).map((c) =>
        value(c.email, c.name ?? undefined),
      );
    default:
      // An operator we do not know how to complete (`keyword:`, `before:`): no value
      // suggestions rather than wrong ones. Dates and IMAP keywords are open sets.
      return [];
  }
}

/** Loaded senders first, then fetched contacts, one row per address (case-folded). */
function mergeContacts(
  a: SuggestSources["contacts"],
  b: SuggestSources["contacts"],
): SuggestSources["contacts"] {
  const seen = new Set<string>();
  const out: Array<{ email: string; name?: string | null }> = [];
  for (const c of [...a, ...b]) {
    const fold = c.email.trim().toLowerCase();
    if (fold === "" || seen.has(fold)) continue;
    seen.add(fold);
    out.push(c);
  }
  return out;
}

function rankedContacts(
  contacts: SuggestSources["contacts"],
  partial: string,
): SuggestSources["contacts"] {
  if (partial === "") return contacts.slice(0, MAX_SUGGESTIONS);
  const scored = contacts
    .map((c) => {
      const name = c.name ?? "";
      if (startsWithFold(c.email, partial)) return { c, rank: 0 };
      if (startsWithFold(name, partial)) return { c, rank: 1 };
      if (containsFold(c.email, partial) || containsFold(name, partial)) return { c, rank: 2 };
      return null;
    })
    .filter((s): s is { c: SuggestSources["contacts"][number]; rank: number } => s !== null);
  scored.sort((a, b) => a.rank - b.rank);
  return scored.map((s) => s.c);
}

