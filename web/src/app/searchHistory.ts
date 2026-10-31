// SPDX-License-Identifier: AGPL-3.0-only

// RECENT SEARCHES (#57), kept in this browser and nowhere else.
//
// THE STORAGE CHOICE IS THE PRIVACY DECISION, so it is made here explicitly rather than
// defaulted into. A search history is a record of what someone went looking for in their
// own mail — who they were checking on, what they were worried about, what they were
// trying to find and could not. That is more revealing than most of the mail it searches.
//
// So it is `localStorage`, like theme and density (prefs.ts): it never leaves the device,
// it is not in the database, it is not in any backup, it appears in no export, and it
// syncs to nothing. The cost is real and accepted — a search remembered on the laptop is
// not offered on the phone. Making it sync would mean storing a per-user search log
// server-side, which is a thing worth having a reason for, and "so the phone remembers
// too" is not a good enough one.
//
// It is also CLEARABLE from the search box itself, in one action, because a history you
// cannot clear is a log the user did not agree to keep.

const HISTORY_KEY = "mailroom.searchHistory";

/** How many queries to keep. Small on purpose: the value of a history is the last few
 * things you looked for, and a long one is both a bigger record to hold and a list nobody
 * scrolls. */
export const HISTORY_LIMIT = 12;

/** Longest query to remember. A pasted wall of text is not a search someone will want
 * offered back, and there is no reason to hold it. */
const MAX_QUERY_LENGTH = 200;

/** The remembered queries, most recent first. Returns [] when storage is unavailable —
 * a browser with storage disabled, or a private window, must lose the feature and not the
 * app. */
export function loadSearchHistory(): string[] {
  try {
    const raw = window.localStorage.getItem(HISTORY_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    // Validated element by element rather than trusted: this value is editable by
    // anyone with devtools, and a non-string in the list would break the suggester.
    return parsed
      .filter((v): v is string => typeof v === "string" && v.trim() !== "")
      .slice(0, HISTORY_LIMIT);
  } catch {
    return [];
  }
}

/**
 * Records a query, newest first, de-duplicated, and returns the new list.
 *
 * De-duplication is case-insensitive and ignores surrounding whitespace, because
 * `from:Alice` and `from:alice ` are the same search and offering both back wastes two
 * rows on one memory. The form KEPT is the one just typed — the most recent spelling is
 * the one the user is currently thinking in.
 */
export function rememberSearch(query: string): string[] {
  const q = query.trim();
  if (q === "" || q.length > MAX_QUERY_LENGTH) return loadSearchHistory();
  const fold = q.toLowerCase();
  const next = [q, ...loadSearchHistory().filter((h) => h.trim().toLowerCase() !== fold)].slice(
    0,
    HISTORY_LIMIT,
  );
  save(next);
  return next;
}

/** Forgets everything. */
export function clearSearchHistory(): string[] {
  save([]);
  return [];
}

function save(list: string[]): void {
  try {
    window.localStorage.setItem(HISTORY_KEY, JSON.stringify(list));
  } catch {
    // A full or disabled storage must not break searching. The history is a
    // convenience; losing it silently is the correct failure.
  }
}
