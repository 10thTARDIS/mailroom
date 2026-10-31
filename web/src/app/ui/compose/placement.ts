// SPDX-License-Identifier: AGPL-3.0-only

// WHERE THE COMPOSER SITS (#448, design pass-2 §7).
//
// It was a fixed panel docked to the bottom-right corner — the Gmail idiom — and
// the design replaces that with the reading-pane COLUMN: composing a reply is the
// same act as reading, in the same place, with the list still beside it.
//
//   pane  ≥760px with a reading pane configured (Beside the list). The composer
//         is the grid track ThreadPane uses; the list stays visible; the reading
//         pane is not rendered, because one column has one occupant.
//   main  ≥760px with the reading pane OFF (Focused). There is no second column
//         to take, so the composer takes the whole content area — over the list,
//         opaquely, never a translucent scrim (spec §12).
//   full  <760px. The phone has one pane at a time, and the composer is it.
//
// A DECISION AND NOT PURELY A STYLESHEET, for the reason ui/useMediaQuery.ts
// gives: `pane` and `main` differ in whether focus is TRAPPED (in `pane` the list
// is beside you and reachable; in `main` and `full` the composer has the screen,
// so Tab must not walk out of it into a list nobody can see). That is behaviour,
// not styling, so it needs a value — and the value belongs somewhere a test can
// reach, because the layout itself is not testable in this repo (no jsdom).

import type { PaneLayout } from "../../prefs";

export type ComposePlacement = "pane" | "main" | "full";

/**
 * Where the composer goes.
 *
 * `paneLayout` is the device's reading-pane setting (app/prefs.ts): `vertical` /
 * `horizontal` mean "beside the list", `off` means "focused". `phone` is the
 * <760px media query, which wins over everything — below that width there is only
 * ever one pane on screen.
 */
export function composePlacement(opts: {
  phone: boolean;
  paneLayout: PaneLayout;
}): ComposePlacement {
  if (opts.phone) return "full";
  return opts.paneLayout === "off" ? "main" : "pane";
}

/**
 * Whether focus must be trapped inside the composer.
 *
 * TRUE EXACTLY WHEN THE COMPOSER HAS THE SCREEN. Trapping in `pane` would be
 * wrong twice over: the list is visibly beside the composer, so a Tab that
 * refused to reach it would contradict what the user can see, and a trap is only
 * defensible when escaping it is possible some other way (Escape closes the
 * composer, which discards nothing — the draft is autosaved).
 */
export function trapsFocus(placement: ComposePlacement): boolean {
  return placement !== "pane";
}
