// SPDX-License-Identifier: AGPL-3.0-only

// FINDING A SETTING BY WHAT IT SAYS, NOT BY WHERE IT LIVES (#374 increment 2).
//
// Settings is one page of twelve rows, and the field above them is the only
// affordance that scales: a user who wants "the newsletter rule" or "that
// domain I let images through for" does not know which row it is in, and the
// twelve headings are exactly the words they will not think of. So a query
// matches a row three ways —
//
//   * its NAME            ("appearance")
//   * its STATE LINE      ("dark", "0.6", "7 days")
//   * any of its VALUES   a rule's name, a saved search's name, an area or
//                         label name, a remembered sender, an identity's alias
//
// — and the third one is the reason this file exists at all. Matching only the
// name and the state line would make the search a table of contents, which the
// twelve headings already are; matching values is what makes typing an alias
// find the row that CONTAINS it rather than nothing.
//
// PURE, AND KEPT OUT OF THE COMPONENT ON PURPOSE. Each row hands over its own
// `values` when it is built (SettingsRoute.tsx), so this module never has to
// know what a rule or an area is — and the behaviour that decides whether the
// user finds their setting is testable without rendering anything.

/** What a row has to publish to be findable. The row's own `id` rides along so
 * the caller can act on the result (expand a single match) without a second
 * lookup. */
export interface SearchableRow {
  id: string;
  /** The row's heading, as shown. */
  name: string;
  /** The row's state line, as shown — the facts, already worded. */
  stateText: string;
  /** Everything the row NAMES that is not in its state line: rule names, saved
   * search names, area and label names, allowed senders, aliases. Empty is
   * legitimate (the Data row names nothing) and is not the same as "not
   * measured": a row with no values simply cannot be found by one. */
  values: string[];
}

/** Case- and whitespace-insensitive haystack for one row. Built once per row per
 * query rather than per term, because a query of three terms would otherwise
 * lowercase the same strings three times for every row on the page. */
function haystack(row: SearchableRow): string {
  return [row.name, row.stateText, ...row.values].join(" ").toLowerCase();
}

/**
 * The rows a query matches, in the order they were given.
 *
 * AN EMPTY QUERY MATCHES EVERYTHING — it is not "no matches", it is "nothing
 * asked", and a page that emptied itself when the field was cleared would lose
 * the twelve rows the user came for.
 *
 * EVERY TERM MUST MATCH SOMETHING, any of the three ways. A single term is the
 * plain substring case the design describes; splitting on whitespace is what
 * keeps "dark theme" from finding nothing on a row whose state line says
 * `theme dark` — the words a user types are not in the order the row prints
 * them, and a settings search that only works when they are is a search people
 * stop using after the first miss. The terms are ANDed rather than ORed for the
 * same reason: two words are a narrowing, not a widening.
 */
export function filterSettings<T extends SearchableRow>(rows: readonly T[], query: string): T[] {
  const terms = query.toLowerCase().split(/\s+/).filter((t) => t.length > 0);
  if (terms.length === 0) return [...rows];
  return rows.filter((row) => {
    const hay = haystack(row);
    return terms.every((term) => hay.includes(term));
  });
}
