// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE PHONE'S ROW IS FOUR BUTTONS AND MORE (#375, #798). #646 added Reply all
// to both forms of the row. On a phone that sixth button made the row 416 px
// wide, and the whole reading pane scrolled sideways under it. Reply all stays
// one tap away, as More's first item. These tests pin both halves: it LEFT the
// phone's row, and it is still REACHABLE there. The desktop row keeps it.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { PaneToolbar } from "./PaneToolbar";
import type { PaneToolbarProps } from "./PaneToolbar";

let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});

afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

function render(compact: boolean, onReplyAll = vi.fn()): void {
  const props: PaneToolbarProps = {
    inTray: false,
    muted: false,
    snoozed: false,
    compact,
    onBack: vi.fn(),
    onArchive: vi.fn(),
    onTray: vi.fn(),
    onSnooze: vi.fn(),
    onMute: vi.fn(),
    onReply: vi.fn(),
    onReplyAll,
    menu: [{ key: "trash", label: "Delete", onSelect: vi.fn() }],
  };
  flushSync(() => root.render(<PaneToolbar {...props} />));
}

/** The row's own buttons, by their text, excluding the menu's items. */
function rowLabels(): string[] {
  const row = host.querySelector('[role="toolbar"]');
  return [...(row?.querySelectorAll<HTMLButtonElement>(":scope > button, :scope > .pane-more > button") ?? [])].map(
    (b) => (b.textContent ?? "").trim(),
  );
}

function openMore(): HTMLButtonElement[] {
  const more = host.querySelector<HTMLButtonElement>('.pane-more > button[aria-haspopup="menu"]');
  flushSync(() => more?.click());
  return [...host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')];
}

describe("PaneToolbar: Reply all on the phone (#798)", () => {
  it("is not a button in the phone's row", () => {
    render(true);
    expect(rowLabels()).not.toContain("Reply all");
    expect(rowLabels().filter((l) => l !== "←")).toEqual(["Archive", "In-tray", "Snooze", "Reply", "More ▾"]);
  });

  it("is More's first item on the phone, and it replies to all", () => {
    const onReplyAll = vi.fn();
    render(true, onReplyAll);
    const items = openMore();
    expect(items.map((i) => i.textContent)).toEqual(["Reply all", "Mute conversation", "Delete"]);
    flushSync(() => items[0]?.click());
    expect(onReplyAll).toHaveBeenCalledTimes(1);
  });

  it("stays in the desktop row, and is not repeated in More there", () => {
    render(false);
    expect(rowLabels()).toContain("Reply all");
    expect(openMore().map((i) => i.textContent)).toEqual(["Delete"]);
  });
});
