// SPDX-License-Identifier: AGPL-3.0-only

// THE PHONE'S BOTTOM BAR (#375 increment 7).
//
//   Inbox · In-tray · All mail · Search · Compose
//
// WORDS, NOT ICONS. The same argument the reading pane's toolbar settled
// (ui/pane/PaneToolbar.tsx): a row of five glyphs is five things to learn, and
// the two that would be hardest to draw here — "in-tray" and "all mail" — are
// exactly the two that are not standard anywhere else. The words fit; five
// short ones at 13px fit a 320px screen with room to spare.
//
// IT IS NOT THE RAIL, AND IT DOES NOT REPLACE IT. These five are the
// destinations reached constantly; the rail (`.sidebar`) still holds the
// labels, the areas, the folders and settings, and below 760px it is a PANE
// reached through the header's ☰ (Shell.tsx's `navOpen` → `data-pane="nav"`).
// So the two coexist by covering different things: the bar is the frequent
// journeys, the rail is the whole map. The bar stays on screen while the rail
// is open, which is what makes the rail a place you can leave with one tap.
//
// IT IS A FLEX CHILD, NOT `position: fixed`, AND THAT IS WHY NOTHING IS EVER
// COVERED. `.app` is a `100dvh` flex column whose scrolling happens INSIDE
// `.workspace`, so a final child of `.app` is already pinned to the bottom of
// the window in the only sense a user can perceive — and it takes its own
// space, so no list, no rail and no reading pane needs a compensating
// `padding-bottom` that would drift out of step with the bar's height. The
// safe-area inset is padding ON the bar, so the taps land above the home
// indicator and the bar's background still runs to the bottom edge.

import type { JSX } from "react";

import type { Route } from "../../core/routes";
import { t } from "../i18n";

export interface BottomBarProps {
  /** The route the destinations are compared against — the LIST route, so a
   * settings sheet over the desk still shows Inbox as where you are. */
  route: Route;
  /** Navigating keeps the current scope; the caller adds the account. */
  onNavigate: (to: Route) => void;
  /** Search is not a destination but the header's field, which on a phone is
   * above the fold and easy to miss. */
  onSearch: () => void;
  onCompose: () => void;
}

export function BottomBar({ route, onNavigate, onSearch, onCompose }: BottomBarProps): JSX.Element {
  const entry = (
    key: string,
    label: string,
    current: boolean,
    onClick: () => void,
  ): JSX.Element => (
    <button
      key={key}
      type="button"
      className={current ? "bottom-bar-item is-current" : "bottom-bar-item"}
      // `page`, not `true`: these are places, and the one you are on is a page.
      // Never on Search, which is a control rather than a destination.
      aria-current={current ? "page" : undefined}
      onClick={onClick}
    >
      {label}
    </button>
  );

  return (
    <nav className="bottom-bar" aria-label={t("bottomBar.label")}>
      {entry("inbox", t("nav.inbox"), route.page === "inbox", () =>
        onNavigate({ page: "inbox", tab: null }),
      )}
      {entry("tray", t("rail.tray"), route.page === "tray", () => onNavigate({ page: "tray" }))}
      {/* All mail is not the archive: it is where mail IS (core/views.ts). The
          `?area=`/`?label=` forms of the same page are a filtered all-mail, so
          they are not this entry. */}
      {entry(
        "all",
        t("nav.all"),
        route.page === "all" && !route.area && !route.label,
        () => onNavigate({ page: "all" }),
      )}
      {entry("search", t("bottomBar.search"), false, onSearch)}
      {/* The composer IS on screen when `compose` is set — it is a route
          parameter over whatever list you were on — so it carries the current
          marker while it is open. */}
      {entry("compose", t("action.compose"), route.compose !== undefined, onCompose)}
    </nav>
  );
}
