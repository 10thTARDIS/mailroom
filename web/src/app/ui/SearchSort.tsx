// SPDX-License-Identifier: AGPL-3.0-only

// NEWEST FIRST / BEST MATCH (#724) — the order of a search's results.
//
// Two toggle buttons in a named group rather than a <select>: there are exactly
// two orders, both visible at once is one click instead of two, and
// `aria-pressed` states which one is on in the accessibility tree. Choosing one
// is a navigation (the sort is part of the search's URL, core/routes.ts), so a
// reload or a shared link shows the same order, and Back undoes it.
//
// Best match is the server's order (Email/query `relevance`, internal/store
// EmailQuery.SortByRank) and pages like any other: "Load more" re-asks the same
// sequence for a longer window.

import type { JSX } from "react";

import type { View } from "../../core/views";
import { t } from "../i18n";

type SearchView = Extract<View, { kind: "search" }>;

export function SearchSort({ view, setView }: { view: SearchView; setView: (v: View) => void }): JSX.Element {
  const relevance = view.sort === "relevance";
  const pick = (want: boolean): void => {
    if (want === relevance) return;
    const { sort: _drop, ...rest } = view;
    setView(want ? { ...rest, sort: "relevance" } : rest);
  };
  return (
    <div className="search-sort" role="group" aria-label={t("search.sort.label")}>
      <button type="button" className="search-sort-option" aria-pressed={!relevance} onClick={() => pick(false)}>
        {t("search.sort.newest")}
      </button>
      <button type="button" className="search-sort-option" aria-pressed={relevance} onClick={() => pick(true)}>
        {t("search.sort.relevance")}
      </button>
    </div>
  );
}
