// SPDX-License-Identifier: AGPL-3.0-only

import type { View } from "../../core/views";

/**
 * What the search box should show when the view changes, or null to leave it
 * alone (#724).
 *
 * The box mirrors the view: leaving a search empties it, arriving at one (a
 * pasted URL, a saved search, the back button) fills it. EXCEPT a search the
 * box itself started while typing: live results navigate after a pause, and by
 * the time that view arrives the user may have typed more — writing the view's
 * query back would eat those keystrokes. `liveQuery` is the query the box last
 * asked for; arriving at exactly that leaves the box as it is.
 */
export function searchBoxTextFor(view: View, liveQuery: string | null): string | null {
  if (view.kind === "search" && liveQuery !== null && view.query === liveQuery) return null;
  return view.kind === "search" ? view.query : "";
}
