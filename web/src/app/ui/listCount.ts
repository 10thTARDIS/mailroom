// SPDX-License-Identifier: AGPL-3.0-only

// The list's count line, "N of M" — and, when the search index stopped at its hit
// limit (#722's searchCapped), "N of at least M" (#724). A capped search's total
// counts only what the index handed over, so printing it as the total would be a
// lower bound presented as a fact (the #155 shape). One function, so the list and
// its tests cannot disagree about which sentence a capped search gets.

import { t } from "../i18n";

export function listCountText(shown: number, total: number, searchCapped: number | null): string {
  return searchCapped !== null
    ? t("list.countOfAtLeast", { shown, total })
    : t("list.countOf", { shown, total });
}

/** Why the count is a floor, for the count's title — or undefined when it is not. */
export function listCountExplain(total: number, searchCapped: number | null): string | undefined {
  return searchCapped !== null ? t("list.countBoundExplain", { total }) : undefined;
}
