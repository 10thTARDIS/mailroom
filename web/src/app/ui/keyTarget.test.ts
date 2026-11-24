// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

import { describe, expect, it } from "vitest";

import { activatesOnEnter } from "./keyTarget";

// Built with createElement, not innerHTML: cmd/mailroom's security test forbids
// .innerHTML anywhere in web/, tests included.
const el = (tag: string, attrs: Record<string, string> = {}, text = "x"): Element => {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  e.textContent = text;
  return e;
};

describe("activatesOnEnter", () => {
  it("is true for a button, a summary, a link with an href, and the self-activating roles", () => {
    expect(activatesOnEnter(el("button", {}, "Forget what was learned"))).toBe(true);
    expect(activatesOnEnter(el("summary", {}, "Compare the keys"))).toBe(true);
    expect(activatesOnEnter(el("a", { href: "#/all" }))).toBe(true);
    expect(activatesOnEnter(el("span", { role: "button", tabindex: "0" }))).toBe(true);
    expect(activatesOnEnter(el("li", { role: "menuitem" }))).toBe(true);
    expect(activatesOnEnter(el("span", { role: "tab" }))).toBe(true);
  });

  it("is false for the list, for nothing, and for things that do not click themselves", () => {
    // The grid container is not a button: Enter on it must still open the row.
    expect(activatesOnEnter(el("ul", { role: "grid", tabindex: "0" }, ""))).toBe(false);
    expect(activatesOnEnter(el("a", {}, "no href"))).toBe(false);
    expect(activatesOnEnter(el("div", {}, "text"))).toBe(false);
    expect(activatesOnEnter(null)).toBe(false);
    expect(activatesOnEnter(window)).toBe(false);
  });
});
