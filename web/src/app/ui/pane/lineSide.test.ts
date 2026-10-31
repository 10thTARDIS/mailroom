// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { lineSideSentence } from "./lineSide";

describe("lineSideSentence", () => {
  it("says the surface sentence for surface", () => {
    expect(lineSideSentence("surface")).toBe("pane.lineSide.surface");
  });

  it("says the drawer sentence for drawer", () => {
    expect(lineSideSentence("drawer")).toBe("pane.lineSide.drawer");
  });

  it("says nothing when the score still decides", () => {
    expect(lineSideSentence(null)).toBeNull();
  });

  it("says nothing for an older server that never reported one", () => {
    expect(lineSideSentence(undefined)).toBeNull();
  });
});
