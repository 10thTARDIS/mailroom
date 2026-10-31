// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Learned placement's off switch in Settings (#714): a checkbox whose
// accessible name is its visible text (WCAG 2.5.3), reflecting the server's
// DeskPreferences.learnedPlacementEnabled, and writing the flipped value
// through the one DeskPreferences write path.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { DeskPreferences } from "../../../../core/jmap/types";
import { MailStore } from "../../../../core/store";
import type { MailState } from "../../../../core/store";

let state: MailState;
const updateDeskPreferences = vi.fn(async () => true);
vi.mock("../../../state", () => ({
  useApp: () => ({ actions: { updateDeskPreferences } }),
  useMailState: () => state,
}));

import { LearnedPlacementToggle } from "./LearnedPlacementToggle";

function prefs(over: Partial<DeskPreferences>): DeskPreferences {
  return {
    id: "U1",
    lineThreshold: 0.5,
    drawerDefaultDays: 30,
    trayNagDays: 7,
    drawerSweepEnabled: false,
    setupCompletedAt: "2026-10-01T00:00:00Z",
    ...over,
  };
}

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  updateDeskPreferences.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function mount(p: DeskPreferences | null): void {
  const store = new MailStore(null);
  if (p) store.setDeskPreferences(p);
  state = store.getState();
  flushSync(() => root.render(<LearnedPlacementToggle />));
}

function box(): HTMLInputElement | null {
  return host.querySelector('input[type="checkbox"]');
}

describe("LearnedPlacementToggle (#714)", () => {
  it("shows the server's ON as a checked box whose name is its visible text", () => {
    mount(prefs({ learnedPlacementEnabled: true }));
    const input = box();
    expect(input?.checked).toBe(true);
    const label = input?.closest("label");
    // WCAG 2.5.3: the accessible name comes from the wrapping label's text,
    // with no aria-label to diverge from it.
    expect(input?.getAttribute("aria-label")).toBeNull();
    expect(label?.textContent).toBe("Sort new mail into areas by learning from your moves");
  });

  it("switching it off writes learnedPlacementEnabled: false through DeskPreferences", () => {
    mount(prefs({ learnedPlacementEnabled: true }));
    flushSync(() => box()?.click());
    expect(updateDeskPreferences).toHaveBeenCalledTimes(1);
    expect(updateDeskPreferences).toHaveBeenCalledWith({ learnedPlacementEnabled: false }, expect.any(String));
  });

  it("shows OFF as unchecked, and switching it on writes true", () => {
    mount(prefs({ learnedPlacementEnabled: false }));
    expect(box()?.checked).toBe(false);
    flushSync(() => box()?.click());
    expect(updateDeskPreferences).toHaveBeenCalledWith({ learnedPlacementEnabled: true }, expect.any(String));
  });

  it("draws nothing against a server that does not send the switch", () => {
    mount(prefs({}));
    expect(box()).toBeNull();
    mount(null);
    expect(box()).toBeNull();
  });
});
