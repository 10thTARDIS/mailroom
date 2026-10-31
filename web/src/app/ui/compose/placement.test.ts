// SPDX-License-Identifier: AGPL-3.0-only

// Where the composer sits, and whether it traps focus (#448).

import { describe, expect, it } from "vitest";

import { composePlacement, trapsFocus } from "./placement";

describe("composePlacement", () => {
  it("takes the reading-pane column when a pane is configured", () => {
    expect(composePlacement({ phone: false, paneLayout: "vertical" })).toBe("pane");
    expect(composePlacement({ phone: false, paneLayout: "horizontal" })).toBe("pane");
  });

  it("takes the main area when the reading pane is off", () => {
    expect(composePlacement({ phone: false, paneLayout: "off" })).toBe("main");
  });

  it("is full screen on the phone, whatever the pane setting says", () => {
    // The <760px layout shows one pane at a time; a "pane column" there is a
    // column that does not exist.
    expect(composePlacement({ phone: true, paneLayout: "vertical" })).toBe("full");
    expect(composePlacement({ phone: true, paneLayout: "off" })).toBe("full");
  });
});

describe("trapsFocus", () => {
  it("traps when the composer has the screen", () => {
    expect(trapsFocus("main")).toBe(true);
    expect(trapsFocus("full")).toBe(true);
  });

  // The control: a trap in the pane column would refuse Tab access to a list the
  // user can plainly see beside it.
  it("does not trap while the list is beside it", () => {
    expect(trapsFocus("pane")).toBe(false);
  });
});
