// SPDX-License-Identifier: AGPL-3.0-only

// The rule-8 sentence (ADR-054) in the reader sits on the warn palette, and the
// browser lane (scripts/a11y-check.cjs, keychange surfaces) measured it at 5.3:1
// because `.hint` gave it the muted colour. It has to take the block's colour.
// A source scan, because jsdom has no layout and so no contrast; the lane is the
// measurement and this is the guard that keeps the cause from coming back.

import { describe, expect, it } from "vitest";

import stylesCss from "../../styles.css?raw";

/** The declarations of the rule whose selector is exactly `selector`, or null. */
function ruleBody(css: string, selector: string): string | null {
  const noComments = css.replace(/\/\*[\s\S]*?\*\//g, "");
  for (const m of noComments.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = (m[1] ?? "").split(",").map((s) => s.trim());
    if (selectors.includes(selector)) return m[2] ?? "";
  }
  return null;
}

describe("the pin-voucher sentence inside the reader's warn block", () => {
  it("takes the block's colour, not the muted hint colour", () => {
    const body = ruleBody(stylesCss, ".signature-line-pin-voucher .pin-voucher-warning");
    expect(body, "no rule for the sentence inside .signature-line-pin-voucher").not.toBeNull();
    expect(body).toMatch(/color:\s*inherit/);
  });

  it("the block itself is the warn palette (so inheriting is the right colour)", () => {
    const body = ruleBody(stylesCss, ".signature-line-pin-voucher");
    expect(body).toMatch(/color:\s*var\(--warn-fg\)/);
    expect(body).toMatch(/background:\s*var\(--warn-bg\)/);
  });

  it("the scan can fail: the helper finds nothing for a selector that has no rule", () => {
    expect(ruleBody(stylesCss, ".no-such-rule-150")).toBeNull();
  });
});
