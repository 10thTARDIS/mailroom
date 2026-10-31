// SPDX-License-Identifier: AGPL-3.0-only

// The failure this guards is the quiet one: a page that says `changed` beside a
// control nobody touched. It reads as harmless and it destroys the only thing the
// annotation is for — being able to trust that an unannotated control is
// untouched. So the first test asserts the EMPTY result for the defaults, and
// every later one names exactly one control.

import { describe, expect, it } from "vitest";

import { APPEARANCE_DEFAULTS, changedControls, isChanged } from "./changed";
import type { AppearanceState } from "./changed";

function state(over: Partial<AppearanceState> = {}): AppearanceState {
  return { ...APPEARANCE_DEFAULTS, ...over };
}

describe("changedControls", () => {
  it("reports nothing for the shipped defaults", () => {
    expect(changedControls(state())).toEqual([]);
  });

  it("names the defaults the rest of the app falls back to", () => {
    // Pinned rather than assumed: prefs.ts's loadTheme/loadDensity fall back to
    // these, and a default written down twice is a default that drifts.
    expect(APPEARANCE_DEFAULTS).toEqual({
      theme: "system",
      density: "cozy",
      textSize: "m",
      rowLayout: "oneLine",
      messageOpens: "beside",
      splitPct: 58,
    });
  });

  it("names the theme, and only the theme, when the theme is chosen", () => {
    expect(changedControls(state({ theme: "dark" }))).toEqual(["theme"]);
    expect(changedControls(state({ theme: "light" }))).toEqual(["theme"]);
  });

  it("names the density only when it is not the default", () => {
    // Both directions from the default, because "cozy" sits between the other
    // two and an off-by-one comparison would catch only one side.
    expect(changedControls(state({ density: "comfortable" }))).toEqual(["density"]);
    expect(changedControls(state({ density: "compact" }))).toEqual(["density"]);
    expect(changedControls(state({ density: "cozy" }))).toEqual([]);
  });

  it("names messageOpens when the reading pane is off", () => {
    expect(changedControls(state({ messageOpens: "focused" }))).toEqual(["messageOpens"]);
  });

  it("names the split when the divider has moved", () => {
    expect(changedControls(state({ splitPct: 42 }))).toEqual(["split"]);
  });

  it("does not call a rounding artefact a change", () => {
    // The percent comes from dividing by a measured container (prefs.ts
    // pxToPct), so a value a hair off 58 is the default and a drag nobody made.
    expect(changedControls(state({ splitPct: 58.0000001 }))).toEqual([]);
    expect(changedControls(state({ splitPct: 57.6 }))).toEqual([]);
    expect(changedControls(state({ splitPct: 57.4 }))).toEqual(["split"]);
  });

  it("names the text size at every step except the middle one", () => {
    // Both sides of the default, because "m" sits in the middle of five and a
    // comparison written the wrong way round would catch only one direction.
    for (const textSize of ["xs", "s", "l", "xl"] as const) {
      expect(changedControls(state({ textSize }))).toEqual(["textSize"]);
    }
    expect(changedControls(state({ textSize: "m" }))).toEqual([]);
  });

  it("names the row layout only for multi-line", () => {
    expect(changedControls(state({ rowLayout: "multiLine" }))).toEqual(["rowLayout"]);
    expect(changedControls(state({ rowLayout: "oneLine" }))).toEqual([]);
  });

  it("lists several, in the row's own order", () => {
    expect(
      changedControls({
        theme: "dark",
        density: "compact",
        textSize: "xl",
        rowLayout: "multiLine",
        messageOpens: "focused",
        splitPct: 30,
      }),
    ).toEqual(["theme", "density", "textSize", "rowLayout", "messageOpens", "split"]);
  });

  it("isChanged answers per control and agrees with the list", () => {
    const s = state({ density: "compact" });
    expect(isChanged(s, "density")).toBe(true);
    expect(isChanged(s, "theme")).toBe(false);
    expect(isChanged(s, "split")).toBe(false);
  });
});
