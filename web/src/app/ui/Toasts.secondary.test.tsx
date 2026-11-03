// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A toast with a second button (rule suggestion's "Not now"): both are real
// buttons with names, each runs its own callback and dismisses the toast, and
// the toast passes the accessibility scan.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { Toast } from "../state";

const dismissToast = vi.fn();
let toasts: Toast[] = [];
vi.mock("../state", () => ({
  useApp: () => ({ toasts, dismissToast, runUndo: vi.fn(), extendUndo: vi.fn() }),
}));

import { Toasts } from "./Toasts";
import { axeViolations } from "./settings/apikeys/axe.testdata";

let host: HTMLDivElement;
let root: Root;
const run = vi.fn();
const secondRun = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  toasts = [
    {
      id: 7,
      kind: "info",
      message: "Always put mail from news@example.com in News?",
      action: { label: "Make a rule", run },
      secondary: { label: "Not now", run: secondRun },
    },
  ];
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  flushSync(() => root.render(<Toasts />));
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

const button = (name: string): HTMLButtonElement =>
  [...host.querySelectorAll("button")].find((b) => b.textContent === name || b.getAttribute("aria-label") === name)!;

describe("a toast with a secondary button", () => {
  it("renders both as named buttons inside the live region", () => {
    expect(host.querySelector('[role="status"]')).not.toBeNull();
    expect(button("Make a rule")).toBeDefined();
    expect(button("Not now")).toBeDefined();
    expect(button("Not now").tagName).toBe("BUTTON");
  });

  it("the primary runs its own callback and dismisses", () => {
    flushSync(() => button("Make a rule").click());
    expect(run).toHaveBeenCalledTimes(1);
    expect(secondRun).not.toHaveBeenCalled();
    expect(dismissToast).toHaveBeenCalledWith(7);
  });

  it("the secondary runs its own callback and dismisses", () => {
    flushSync(() => button("Not now").click());
    expect(secondRun).toHaveBeenCalledTimes(1);
    expect(run).not.toHaveBeenCalled();
    expect(dismissToast).toHaveBeenCalledWith(7);
  });

  it("has no accessibility violations", async () => {
    expect(await axeViolations()).toEqual([]);
  });
});
