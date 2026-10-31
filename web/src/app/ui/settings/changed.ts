// SPDX-License-Identifier: AGPL-3.0-only

// WHICH APPEARANCE CONTROLS THE USER HAS ACTUALLY MOVED (#374 increment 2).
//
// A DEFAULT SAYS NOTHING, and that is the design rule this module exists to
// enforce. A settings page that annotates every control equally tells you
// nothing about your own configuration: the useful question is not "what is the
// theme?" — the screen already answers that — it is "what did I change, and what
// is still whatever it shipped as?". So the row prints `changed` beside exactly
// the controls that differ, and offers a per-control reset for those and only
// those.
//
// THE DEFAULTS ARE WRITTEN DOWN HERE, ONCE, and they are the same four values
// the rest of the app already falls back to:
//
//   theme        `system`   — prefs.ts loadTheme()'s fallback
//   textSize     `m`        — prefs.ts DEFAULT_TEXT_SIZE, which is 14px: the
//                             size this app has always drawn at, so the middle
//                             step is where everyone who never opens the
//                             control already is
//   rowLayout    `oneLine`  — the single line, redrawn (#514) but still one line
//   density      `cozy`     — prefs.ts loadDensity()'s fallback, which is the
//                             server's `normal` (densityToServer); the row prints
//                             the server's word because the design prints the
//                             SYNCED vocabulary, but the comparison is done in
//                             this app's own, where the value lives
//   messageOpens `beside`   — a reading pane, which is what the app boots with
//   splitPct     58         — the list's share of the content area
//
// ⚠ THE SPLIT IS COMPARED AS AN INTEGER PERCENT, not as pixels. The synced value
// is a percentage (prefs.ts pctToPx says why: a laptop's 380px list is a phone's
// whole screen), and a device that converted to pixels and back would round
// itself off the default and report a change nobody made.
//
// PURE, so "does the page claim I changed something I did not?" is a unit test
// rather than a screenshot.

import { DEFAULT_ROW_LAYOUT, DEFAULT_TEXT_SIZE } from "../../prefs";
import type { Density, RowLayout, TextSize, Theme } from "../../prefs";

/** The six controls the Appearance row owns. `split` is the divider position;
 * the ORIENTATION (beside-left vs beside-below) is deliberately not here — it is
 * device-only, has no synced default to differ from, and lives behind the row's
 * `more`. */
export type AppearanceControl =
  | "theme"
  | "density"
  | "textSize"
  | "rowLayout"
  | "messageOpens"
  | "split";

export interface AppearanceState {
  theme: Theme;
  /** This app's own vocabulary (comfortable / cozy / compact), not the server's
   * (relaxed / normal / compact). prefs.ts owns the mapping between them. */
  density: Density;
  /** The whole app's text scale (#514). Server-exact words — there is no
   * mapping here, unlike density. */
  textSize: TextSize;
  /** One line per message, or three (#514). */
  rowLayout: RowLayout;
  messageOpens: "beside" | "focused";
  /** The LIST pane's share of the content area, as a whole percent. */
  splitPct: number;
}

export const APPEARANCE_DEFAULTS: Readonly<AppearanceState> = {
  theme: "system",
  density: "cozy",
  // ⚠ IMPORTED, NOT RESTATED. prefs.ts already has to know what the defaults
  // are (loadTextSize's fallback, and the guard that keeps an unknown server
  // word off `<html>`), and "a default written down twice is a default that
  // drifts" is the note the density line above already carries. Density
  // predates this and stays literal only because its two vocabularies make the
  // shared constant ambiguous.
  textSize: DEFAULT_TEXT_SIZE,
  rowLayout: DEFAULT_ROW_LAYOUT,
  messageOpens: "beside",
  splitPct: 58,
};

/**
 * The controls that differ from their default, in the order the row renders
 * them.
 *
 * An empty array is the ordinary case for a fresh account and means exactly
 * that: nothing here has been touched, so nothing is annotated and there is
 * nothing to reset.
 */
export function changedControls(state: AppearanceState): AppearanceControl[] {
  const out: AppearanceControl[] = [];
  if (state.theme !== APPEARANCE_DEFAULTS.theme) out.push("theme");
  if (state.density !== APPEARANCE_DEFAULTS.density) out.push("density");
  if (state.textSize !== APPEARANCE_DEFAULTS.textSize) out.push("textSize");
  if (state.rowLayout !== APPEARANCE_DEFAULTS.rowLayout) out.push("rowLayout");
  if (state.messageOpens !== APPEARANCE_DEFAULTS.messageOpens) out.push("messageOpens");
  // Rounded before comparing: the percent arrives from a division by a measured
  // container (prefs.ts pxToPct), so 58.0000001 is the DEFAULT and must not be
  // reported as a change the user made with a drag they never performed.
  if (Math.round(state.splitPct) !== APPEARANCE_DEFAULTS.splitPct) out.push("split");
  return out;
}

/** Whether one named control differs — the form the row's per-control badge
 * wants. A thin wrapper rather than a second implementation, so the badge and
 * the row's summary can never disagree about what "changed" means. */
export function isChanged(state: AppearanceState, control: AppearanceControl): boolean {
  return changedControls(state).includes(control);
}
