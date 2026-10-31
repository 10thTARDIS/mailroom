// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE SELECTION TOOLBAR'S IMPORTANCE BUTTON (#921). The label names the verb
// the bar is about to do: "Mark as important" while the selection is not all
// important, and "Not important" only when every selected message already is
// — so the bar can never offer to un-mark a selection it was built to mark.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { SelectionToolbar } from "./SelectionToolbar";

const onImportant = vi.fn();

let root: Root;
let host: HTMLElement;

function render(allImportant: boolean): void {
  flushSync(() =>
    root.render(
      <SelectionToolbar
        count={2}
        areas={[]}
        refileOpen={false}
        onArchive={() => {}}
        onTray={() => {}}
        onMarkRead={() => {}}
        onMarkUnread={() => {}}
        onImportant={onImportant}
        onLabel={() => {}}
        onToggleRefile={() => {}}
        onRefile={() => {}}
        onClear={() => {}}
        allImportant={allImportant}
      />,
    ),
  );
}

function button(text: string): HTMLElement {
  const found = Array.from(host.querySelectorAll("button")).find((b) => b.textContent === text);
  if (!found) throw new Error("no button " + text);
  return found;
}

function click(target: HTMLElement): void {
  flushSync(() => {
    target.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
}

beforeEach(() => {
  onImportant.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

describe("SelectionToolbar importance button (#921)", () => {
  it("says 'Mark as important' while the selection is not all important, and the click invokes the verb once", () => {
    render(false);
    expect(host.textContent).not.toContain("Not important");
    click(button("Mark as important"));
    expect(onImportant).toHaveBeenCalledTimes(1);
  });

  it("says 'Not important' — and not the other — when every selected message already is", () => {
    render(true);
    expect(host.textContent).not.toContain("Mark as important");
    click(button("Not important"));
    expect(onImportant).toHaveBeenCalledTimes(1);
  });
});
