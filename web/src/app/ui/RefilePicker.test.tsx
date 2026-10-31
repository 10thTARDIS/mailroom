// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE READING PANE'S RE-FILE PICKER (#921). One item per area; an area whose
// label has not loaded is offered DISABLED with the reason; a pick hands the
// area id to the route BEFORE the picker goes away; Escape closes without
// picking anything.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import { RefilePicker } from "./RefilePicker";
import type { RefileTarget } from "./desk/SelectionToolbar";

const BLOCKED = "Shopping's label has not loaded yet, so mail cannot be filed there right now.";

const areas: RefileTarget[] = [
  { id: "a1", name: "Work", blocked: null },
  { id: "a2", name: "Shopping", blocked: BLOCKED },
  { id: "a3", name: "News", blocked: null },
];

const onPick = vi.fn();
const onClose = vi.fn();

let root: Root;
let host: HTMLElement;

function render(list: RefileTarget[] = areas): void {
  flushSync(() => root.render(<RefilePicker areas={list} onPick={onPick} onClose={onClose} />));
}

function item(name: string): HTMLButtonElement {
  const found = Array.from(host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')).find(
    (b) => b.textContent === name,
  );
  if (!found) throw new Error("no item " + name);
  return found;
}

function click(target: HTMLElement): void {
  flushSync(() => {
    target.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
}

function escape(): void {
  flushSync(() => {
    host
      .querySelector(".modal-backdrop")!
      .dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
  });
}

beforeEach(() => {
  onPick.mockClear();
  onClose.mockClear();
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

describe("RefilePicker (#921)", () => {
  it("offers one item per area, named, in the given order", () => {
    render();
    const names = Array.from(host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')).map(
      (b) => b.textContent,
    );
    expect(names).toEqual(["Work", "Shopping", "News"]);
  });

  it("offers an area it cannot file into, disabled, with the reason", () => {
    render();
    const shopping = item("Shopping");
    expect(shopping.disabled).toBe(true);
    expect(shopping.title).toBe(BLOCKED);
    expect(item("Work").disabled).toBe(false);
  });

  it("hands the area id to the route on a pick, before it closes", () => {
    const order: string[] = [];
    onPick.mockImplementation(() => order.push("pick"));
    onClose.mockImplementation(() => order.push("close"));
    render();
    click(item("Work"));
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick).toHaveBeenCalledWith("a1");
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(order).toEqual(["pick", "close"]);
  });

  it("Escape closes without picking", () => {
    render();
    escape();
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onPick).not.toHaveBeenCalled();
  });
});
