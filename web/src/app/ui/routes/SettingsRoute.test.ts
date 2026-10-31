// SPDX-License-Identifier: AGPL-3.0-only

// THE SETTINGS ✕ CLOSES IN ONE CLICK, WHATEVER IS OPEN — a source-level guard for
// the WIRING, because SettingsRoute cannot be mounted here (no jsdom/RTL in this
// repo — mailroom-web-client-facts) and the defect this pins lived in the wiring,
// not in a pure function: `listRouteOf` (core/routes.test.ts) was already correct
// — "the list underneath, in one step, for any depth" — but `close` called
// `leaveTo(listRoute)` (app/routing.ts), which undoes exactly ONE pushed history
// entry. That is right for a single-layer sheet's ✕ (this file's own
// ShortcutsSheet, one section away from wherever it opened) and wrong for
// Settings past its first section: entering the page, opening a section and
// opening a detail (an account, an area) each push their own entry, so the ✕
// walked back one layer per click instead of leaving in one.
//
// Operator report against the v1.80.0 beta: "When I hit the X on the settings
// modal it's not closing; I need to click it multiple times" — reproduced as
// open Appearance, change a radio, click X (lands on the settings root), click X
// again (lands on the list).
//
// Same technique as outbox.test.ts's "the rail count is not a second copy of the
// rule": assert over the SOURCE that the close handler goes through the one-step
// function rather than the one-layer one, so a future edit that reaches for
// `leaveTo` again — the natural-looking choice, since every OTHER close in this
// file uses it — fails a test instead of costing another multi-click regression.

import { describe, expect, it } from "vitest";

import settingsRouteSource from "./SettingsRoute.tsx?raw";

// Comments stripped, because the assertion is about CODE and this file's own
// explanation of the bug (above, and in SettingsRoute.tsx's `close`) quotes the
// exact expression the negative assertion forbids.
const code = settingsRouteSource
  .replace(/\/\*[\s\S]*?\*\//g, "")
  .replace(/^[^\n]*\/\/.*$/gm, "");

describe("SettingsRoute's ✕ — one click, not one click per layer", () => {
  it("closes through listRouteOf, which answers in a single step for any depth", () => {
    expect(code).toMatch(/navigate\(listRouteOf\(route, listRoute\)/);
  });

  it("does not go back to leaveTo(listRoute) — the one-layer-per-press mechanism the bug was", () => {
    expect(code).not.toMatch(/leaveTo\(listRoute\)/);
  });

  it("the stripper leaves real code alone — otherwise both assertions are vacuous", () => {
    expect(code).toMatch(/const close = \(\): void => \{/);
    expect(code).not.toMatch(/ONE CLICK, WHATEVER IS OPEN/);
  });
});
