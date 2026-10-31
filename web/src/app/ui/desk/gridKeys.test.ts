// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import { gridKeyIntent } from "./gridKeys";

describe("gridKeyIntent — what the desk's grid claims", () => {
  it("moves one row on the unmodified arrows", () => {
    expect(gridKeyIntent({ key: "ArrowDown" })).toEqual({ kind: "move", delta: 1 });
    expect(gridKeyIntent({ key: "ArrowUp" })).toEqual({ kind: "move", delta: -1 });
  });

  it("leaves Shift+Arrow to the line-crossing verb (#464)", () => {
    // The one case that would break a shipped feature if this claimed it: Shift+↑
    // and Shift+↓ are "this belongs on the other side of the line".
    expect(gridKeyIntent({ key: "ArrowUp", shiftKey: true })).toBeNull();
    expect(gridKeyIntent({ key: "ArrowDown", shiftKey: true })).toBeNull();
  });

  it("pages by a direction, leaving the distance to whoever measured the screen", () => {
    expect(gridKeyIntent({ key: "PageDown" })).toEqual({ kind: "page", dir: 1 });
    expect(gridKeyIntent({ key: "PageUp" })).toEqual({ kind: "page", dir: -1 });
  });

  it("goes to the ends on Home/End", () => {
    expect(gridKeyIntent({ key: "Home" })).toEqual({ kind: "edge", to: "first" });
    expect(gridKeyIntent({ key: "End" })).toEqual({ kind: "edge", to: "last" });
  });

  it("opens on Enter and on o", () => {
    expect(gridKeyIntent({ key: "Enter" })).toEqual({ kind: "open" });
    expect(gridKeyIntent({ key: "o" })).toEqual({ kind: "open" });
  });

  it("selects on Space, and extends on Shift+Space", () => {
    expect(gridKeyIntent({ key: " " })).toEqual({ kind: "select", extend: false });
    expect(gridKeyIntent({ key: " ", shiftKey: true })).toEqual({ kind: "select", extend: true });
  });

  it("claims nothing that carries a modifier", () => {
    // Ctrl+Home is "top of the document"; ⌘+↓ is "end of the list" on a Mac;
    // Ctrl/⌘+Z is the app's undo (ui/Shell.tsx). A grid that swallowed these
    // would be one a keyboard user cannot get out of.
    expect(gridKeyIntent({ key: "Home", ctrlKey: true })).toBeNull();
    expect(gridKeyIntent({ key: "ArrowDown", metaKey: true })).toBeNull();
    expect(gridKeyIntent({ key: "End", altKey: true })).toBeNull();
    expect(gridKeyIntent({ key: " ", ctrlKey: true })).toBeNull();
  });

  it("leaves j/k and every other verb to the route's own chain", () => {
    for (const key of ["j", "k", "e", "x", "f", "1", "Tab", "Escape", "?"]) {
      expect(gridKeyIntent({ key })).toBeNull();
    }
  });
});
