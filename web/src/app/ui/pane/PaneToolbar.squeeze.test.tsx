// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE ROW FITS ITS PANE, WHATEVER THE SPLIT (#803). At 1440 px the default split
// gave the reading pane about 493 px, and the eight-button row needed about 574,
// so the whole pane scrolled sideways under it. jsdom does no layout, so the
// row's width is stood in for: each button is 80 px wide (the full row of eight
// needs 640 px, the compact one of six 480) and the pane gives the row
// `paneWidth`. What is under test is the DECISION: when the row's content is
// wider than the row, Reply all and Mute move into More, and they come back when
// the row is wide enough for the full form again, without flapping in between.
// The real widths are measured in a browser by scripts/a11y-check.cjs.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { PaneToolbar } from "./PaneToolbar";

let host: HTMLDivElement;
let root: Root;
let paneWidth = 1000;
const observers: Array<() => void> = [];
const realRO = globalThis.ResizeObserver;
const realScroll = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollWidth");
const realClient = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  observers.length = 0;
  globalThis.ResizeObserver = class {
    constructor(cb: () => void) {
      observers.push(cb);
    }
    observe(): void {}
    disconnect(): void {}
    unobserve(): void {}
  } as unknown as typeof ResizeObserver;
  Object.defineProperty(HTMLElement.prototype, "clientWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return this.getAttribute("role") === "toolbar" ? paneWidth : 0;
    },
  });
  Object.defineProperty(HTMLElement.prototype, "scrollWidth", {
    configurable: true,
    get(this: HTMLElement) {
      if (this.getAttribute("role") !== "toolbar") return 0;
      const n = this.querySelectorAll(":scope > button, :scope > .pane-more > button").length;
      return Math.max(paneWidth, n * 80);
    },
  });
});

afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  globalThis.ResizeObserver = realRO;
  if (realScroll) Object.defineProperty(HTMLElement.prototype, "scrollWidth", realScroll);
  if (realClient) Object.defineProperty(HTMLElement.prototype, "clientWidth", realClient);
});

function render(): void {
  flushSync(() =>
    root.render(
      <PaneToolbar
        inTray={false}
        muted={false}
        snoozed={false}
        onBack={vi.fn()}
        onArchive={vi.fn()}
        onTray={vi.fn()}
        onSnooze={vi.fn()}
        onMute={vi.fn()}
        onReply={vi.fn()}
        onReplyAll={vi.fn()}
        menu={[{ key: "trash", label: "Delete", onSelect: vi.fn() }]}
      />,
    ),
  );
}

function rowLabels(): string[] {
  const row = host.querySelector('[role="toolbar"]');
  return [...(row?.querySelectorAll<HTMLButtonElement>(":scope > button, :scope > .pane-more > button") ?? [])].map(
    (b) => (b.textContent ?? "").trim(),
  );
}

function openMore(): string[] {
  const more = host.querySelector<HTMLButtonElement>('.pane-more > button[aria-haspopup="menu"]');
  flushSync(() => more?.click());
  return [...host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')].map((b) => (b.textContent ?? "").trim());
}

const resize = (w: number): void => {
  paneWidth = w;
  flushSync(() => observers.forEach((cb) => cb()));
};

describe("PaneToolbar: the row measures itself (#803)", () => {
  it("keeps the full row when there is room", () => {
    paneWidth = 1000;
    render();
    expect(rowLabels()).toContain("Reply all");
    expect(rowLabels().some((l) => l.startsWith("Mute"))).toBe(true);
    expect(host.querySelector(".thread-toolbar")?.classList.contains("is-squeezed")).toBe(false);
  });

  it("moves Reply all and Mute into More when the row does not fit, and back when it does", () => {
    paneWidth = 500;
    render();
    expect(rowLabels()).not.toContain("Reply all");
    expect(rowLabels().some((l) => l.startsWith("Mute"))).toBe(false);
    const items = openMore();
    expect(items[0]).toBe("Reply all");
    expect(items.some((l) => l.startsWith("Mute"))).toBe(true);
    expect(host.querySelector(".thread-toolbar")?.classList.contains("is-squeezed")).toBe(true);

    // Still too narrow for the full form (it needs 640): stays squeezed, no flapping.
    resize(600);
    expect(rowLabels()).not.toContain("Reply all");

    resize(700);
    expect(rowLabels()).toContain("Reply all");
    expect(host.querySelector(".thread-toolbar")?.classList.contains("is-squeezed")).toBe(false);
  });
});
