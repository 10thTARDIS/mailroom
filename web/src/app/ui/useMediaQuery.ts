// SPDX-License-Identifier: AGPL-3.0-only

// WHEN THE LAYOUT HAS TO BE A DECISION AND NOT A STYLESHEET (#375 increment 7).
//
// Almost everything the phone layout does is CSS, and that is the right default
// — a media query needs no JavaScript, cannot be out of step with itself, and
// costs nothing at runtime. This exists for the handful of places where the
// phone is a DIFFERENT COMPONENT TREE rather than the same one styled
// differently:
//
//   * the bottom bar EXISTS only on the phone (a `display: none` nav is still
//     five links a screen reader walks past on a desktop);
//   * the in-tray strip is a LINK there and a disclosure here — one tap that
//     navigates versus one that expands, which is behaviour, not styling;
//   * the reading pane's Mute button MOVES INTO the More menu, which is a
//     different set of children, not a hidden one;
//   * the sender popover is placed by arithmetic on the desktop
//     (ui/pane/popover.ts) and by the stylesheet as a bottom sheet on the
//     phone, so the inline coordinates must not be written at all there.
//
// ⚠ THE BREAKPOINT IS WRITTEN TWICE AND CANNOT BE WRITTEN ONCE. `PHONE_QUERY`
// below and the `@media (max-width: 759px)` blocks in app/desk.css and
// app/styles.css are the same number in two languages; CSS cannot read a TS
// constant and TS cannot read a stylesheet. Both files say so where they say
// the number, because the failure if they drift is silent: a bottom bar
// rendered over a two-pane layout, or a phone with no bottom bar at all.

import { useEffect, useState } from "react";

/** The phone breakpoint — the same 759px `app/desk.css` and `app/styles.css`
 * use. One pane at a time below it (Shell.tsx's `data-pane`). */
export const PHONE_QUERY = "(max-width: 759px)";

/** Someone who asked their system for less movement. The reveal behind a swipe
 * is an animation; the swipe itself is not, and only the first is dropped. */
export const REDUCED_MOTION_QUERY = "(prefers-reduced-motion: reduce)";

/**
 * Whether a media query matches, kept current as the window changes.
 *
 * FALSE is the answer wherever `matchMedia` does not exist — a test runner
 * without a DOM, an old browser. That direction is deliberate: false means "the
 * ordinary, wider layout", which renders every control this app has. Defaulting
 * to true would hide the desktop chrome on a runtime we could not measure.
 *
 * The subscription is torn down on unmount and re-made when the query string
 * changes; the queries above are module constants, so in practice that is once.
 */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState<boolean>(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
    return window.matchMedia(query).matches;
  });

  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(query);
    // Re-read on subscribe as well as on change: between the initial state
    // above and this effect the window may already have been resized, and a
    // layout that is one resize behind is one that only corrects itself when
    // the user resizes again.
    setMatches(mq.matches);
    const onChange = (ev: MediaQueryListEvent): void => setMatches(ev.matches);
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [query]);

  return matches;
}
