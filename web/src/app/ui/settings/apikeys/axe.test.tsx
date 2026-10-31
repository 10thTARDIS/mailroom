// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE NEGATIVE CONTROL for axe.testdata.ts: every "passes axe" assertion in the
// API keys tests is worth something only if this helper can report a
// violation in jsdom at all. A nameless button, an unlabelled input and a
// nameless modal dialog must each be reported.

import { afterEach, describe, expect, it } from "vitest";

import { axeViolations } from "./axe.testdata";

afterEach(() => {
  document.body.replaceChildren();
});

/** A dialog holding one element, built through the DOM API. */
function dialog(child: HTMLElement, attrs: Record<string, string>): void {
  const d = document.createElement("div");
  d.setAttribute("role", "dialog");
  for (const [k, v] of Object.entries(attrs)) d.setAttribute(k, v);
  d.appendChild(child);
  document.body.appendChild(d);
}

describe("axeViolations can fail", () => {
  it("reports a nameless button", async () => {
    const b = document.createElement("button");
    b.type = "button";
    dialog(b, { "aria-label": "x" });
    expect((await axeViolations()).join("\n")).toContain("button-name");
  });

  it("reports an unlabelled input", async () => {
    const i = document.createElement("input");
    i.type = "text";
    dialog(i, { "aria-label": "x" });
    expect((await axeViolations()).join("\n")).toContain("label");
  });

  it("reports a modal dialog with no name", async () => {
    const p = document.createElement("p");
    p.textContent = "hi";
    dialog(p, { "aria-modal": "true" });
    expect((await axeViolations()).join("\n")).toContain("aria-dialog-name");
  });
});
