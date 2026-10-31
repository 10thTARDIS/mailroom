// SPDX-License-Identifier: AGPL-3.0-only

// THE DESK'S TWO HALVES (#375 increment 3).
//
//   [ Review first · 6 ] [ Everything else · 37 ]
//
// NOT the category-tab strip (ui/TabStrip.tsx) and deliberately not built from
// it: those five tabs are a filter over the inbox that the user can switch on
// and off, while these two are the desk's own partition — every message is in
// exactly one of them, always, and neither can be disabled. Sharing a component
// would tie two controls with different meanings to one set of styles.
//
// THE URL IS THE TRUTH. A click navigates; the selected tab is read back from
// the route, never from local state. So the back button moves between halves,
// a link to "everything else" is a link someone can send, and a reload lands
// where it left. `saveScope` (app/routing.ts) remembers the last one per
// device, which is what makes a bare `#/inbox` open where you were.
//
// AND IT IS A REAL TABLIST (#447). It had the roles and `aria-selected` and
// nothing else, which is the half of the pattern that is easy: what was missing
// was the keyboard. Both tabs were in the tab order (so Tab walked THROUGH the
// halves on the way to the list) and the arrows did nothing, while `Tab` itself
// was being swallowed inside the list to switch halves — a shortcut that made the
// controls inside the list unreachable and is gone with this issue.
//
//   * ONE TAB STOP: only the selected tab is tabbable, so Tab enters the strip
//     once and leaves it once, which is what makes the whole desk walkable.
//   * ←/→ move between the halves, Home/End to the ends.
//   * AUTOMATIC ACTIVATION — the arrow SELECTS as it moves, rather than requiring
//     Enter. That is the WAI-ARIA choice for a tab whose panel is cheap to show,
//     and here it is doubly right: switching halves is a navigation this app can
//     do instantly from state it already has, and each tab prints its own count,
//     so a reader who lands on "Everything else 37" has already been told what
//     they are about to see.
//   * FOCUS FOLLOWS THE SELECTION, explicitly. The selected tab is read back from
//     the ROUTE, so after an arrow key the tabbable tab is a different element
//     from the one holding focus — the keystroke has to move focus itself or the
//     next arrow would come from the tab the user just left.

import { useRef } from "react";
import type { JSX } from "react";

import { t } from "../../i18n";
import type { DeskTab } from "../../../core/desk";

/**
 * The DOM id of the desk's list.
 *
 * Here rather than in the route because it is shared BETWEEN the tabs and the
 * list: each tab's `aria-controls` points at it, and the list is named by the
 * selected tab in return (ui/desk/VirtualList.tsx says why a grid takes the tab
 * panel's place). Two string literals for one relationship is one of them being
 * renamed on its own, which fails silently — an `aria-controls` naming nothing is
 * not an error anywhere.
 */
export const DESK_GRID_ID = "desk-grid";

/** A tab's own DOM id, which is what the list points at to borrow its name. */
export function deskTabId(tab: DeskTab): string {
  return `desk-tab-${tab}`;
}

/** The halves in the order they are drawn, which is the order the arrows walk. */
const ORDER: DeskTab[] = ["review", "rest"];

export function DeskTabs({
  tab,
  reviewCount,
  restCount,
  onSelect,
}: {
  tab: DeskTab;
  reviewCount: number;
  restCount: number;
  onSelect: (tab: DeskTab) => void;
}): JSX.Element {
  const nodes = useRef<Partial<Record<DeskTab, HTMLButtonElement | null>>>({});

  /** Select a half AND put focus on it — see the header on why the second half
   * cannot be left to the re-render. */
  const go = (next: DeskTab): void => {
    onSelect(next);
    nodes.current[next]?.focus();
  };

  const onKeyDown = (ev: React.KeyboardEvent<HTMLDivElement>): void => {
    const from = ORDER.indexOf(tab);
    let to = -1;
    switch (ev.key) {
      case "ArrowRight":
        to = Math.min(ORDER.length - 1, from + 1);
        break;
      case "ArrowLeft":
        to = Math.max(0, from - 1);
        break;
      case "Home":
        to = 0;
        break;
      case "End":
        to = ORDER.length - 1;
        break;
      default:
        return;
    }
    ev.preventDefault();
    // Stopped so the shell's one window-level chain does not also act on it
    // (ui/Shell.tsx) — Shift is not involved here, but `Home`/`End` reaching the
    // list would move the cursor as well as the half.
    ev.stopPropagation();
    const target = ORDER[to];
    // Already there: the arrow clamps rather than wrapping (the same choice the
    // cursor makes at the ends of the list), so there is nothing to do.
    if (target === undefined || target === tab) return;
    go(target);
  };

  const entry = (value: DeskTab, label: string, count: number): JSX.Element => {
    const selected = tab === value;
    return (
      <button
        type="button"
        role="tab"
        id={deskTabId(value)}
        ref={(node) => {
          nodes.current[value] = node;
        }}
        aria-selected={selected}
        // NAMED EXPLICITLY, rather than left to be computed from "Review first"
        // plus a count pill. Two reasons: what a name computed from a flex row's
        // children looks like ("Review first6"? "Review first 6"?) is
        // implementation-dependent on exactly the boundary between the label and
        // the number, and this name is BORROWED by the list below
        // (`aria-labelledby`), so it is worth being one string this app chose. The
        // visible words are still inside it (WCAG 2.5.3).
        aria-label={t("desk.tab.name", { tab: label, n: count })}
        // The list this tab shows. It is a `role="grid"`, not a `tabpanel` — a
        // grid cannot be both — so the association is `aria-controls` here and
        // the grid borrowing this tab's NAME in return
        // (ui/desk/VirtualList.tsx).
        aria-controls={DESK_GRID_ID}
        tabIndex={selected ? 0 : -1}
        className={selected ? "desk-tab is-selected" : "desk-tab"}
        onClick={() => onSelect(value)}
      >
        {label}
        {/* A real space, which renders nothing inside the flex row but keeps
            the DOM text "Review first 6" rather than "Review first6" — the
            form the name above has to contain (WCAG 2.5.3, axe
            label-content-name-mismatch from axe-core 4.14). */}
        {" "}
        <span className="desk-tab-count">{count}</span>
      </button>
    );
  };

  return (
    <div
      className="desk-tabs"
      role="tablist"
      aria-label={t("desk.tabs.label")}
      onKeyDown={onKeyDown}
    >
      {entry("review", t("desk.tab.review"), reviewCount)}
      {entry("rest", t("desk.tab.rest"), restCount)}
    </div>
  );
}
