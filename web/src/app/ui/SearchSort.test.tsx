// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// The sort toggle and the bounded count (#724).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { View } from "../../core/views";
import { listCountExplain, listCountText } from "./listCount";
import { SearchSort } from "./SearchSort";

let root: Root;
let host: HTMLElement;
beforeEach(() => {
  host = document.createElement("div");
  document.body.replaceChildren(host);
  root = createRoot(host);
});
afterEach(() => flushSync(() => root.unmount()));

describe("SearchSort", () => {
  const buttons = (): HTMLButtonElement[] => [...host.querySelectorAll<HTMLButtonElement>(".search-sort button")];

  it("is a named group of two pressed-state buttons, newest first by default", () => {
    const setView = vi.fn();
    flushSync(() => root.render(<SearchSort view={{ kind: "search", query: "invoice" }} setView={setView} />));
    const group = host.querySelector('[role="group"]');
    expect(group?.getAttribute("aria-label")).toBe("Sort search results");
    expect(buttons().map((b) => [b.textContent, b.getAttribute("aria-pressed")])).toEqual([
      ["Newest first", "true"],
      ["Best match", "false"],
    ]);
    flushSync(() => buttons()[1]?.click());
    expect(setView).toHaveBeenCalledWith({ kind: "search", query: "invoice", sort: "relevance" });
  });

  it("switching back drops the sort and keeps everything else about the search", () => {
    const setView = vi.fn();
    const view: View = { kind: "search", query: "invo", prefix: true, sort: "relevance" };
    flushSync(() => root.render(<SearchSort view={view as Extract<View, { kind: "search" }>} setView={setView} />));
    expect(buttons()[1]?.getAttribute("aria-pressed")).toBe("true");
    flushSync(() => buttons()[0]?.click());
    expect(setView).toHaveBeenCalledWith({ kind: "search", query: "invo", prefix: true });
    // Choosing the order already on is not a navigation.
    setView.mockClear();
    flushSync(() => buttons()[1]?.click());
    expect(setView).not.toHaveBeenCalled();
  });
});

describe("the count shows a capped search as a bound", () => {
  it("says 'at least' when the index stopped at its limit, and never the bare total", () => {
    expect(listCountText(50, 500, 500)).toBe("50 of at least 500");
    expect(listCountText(50, 500, 500)).not.toBe(listCountText(50, 500, null));
    expect(listCountExplain(500, 500)).toContain("stopped after 500");
  });

  it("is the plain count when nothing was capped", () => {
    expect(listCountText(50, 120, null)).toBe("50 of 120");
    expect(listCountExplain(120, null)).toBeUndefined();
  });
});
