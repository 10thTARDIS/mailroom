// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// THE CONSENT PAGE, UNDER AXE (ADR-050 section 5, increment 2).
//
// The consent and error pages are rendered by the server (no script, one inline
// style), so they are not part of the app this lane normally lints. Their real
// output is committed as golden files (internal/oauthserver/testdata, checked
// byte for byte by the Go tests), and this runs axe, the project's pinned
// engine, over those exact files, as whole pages: landmarks, one h1, the
// document title and language, every control labelled, and the rest of WCAG 2 A/AA.
//
// WHAT jsdom CANNOT SEE, said here so the claim is as big as the check: it has
// no layout, so colour contrast and target size are not measured. Contrast is
// checked on the page's palette by the Go test TestConsentPaletteContrast
// (both colour schemes, WCAG AA); the browser accessibility lane measures the
// real rendering.
//
// THE CONTROLS: a page that breaks one rule at a time must be reported by the
// rule that names it. Without these, a green result could mean "axe saw nothing".

import axe from "axe-core";
import { afterEach, describe, expect, it } from "vitest";

import errorPage from "../../../../../../internal/oauthserver/testdata/error.html?raw";
import full from "../../../../../../internal/oauthserver/testdata/consent-full.html?raw";
import readonly from "../../../../../../internal/oauthserver/testdata/consent-readonly.html?raw";

// The page is parsed by the DOM's own parser and adopted whole (no innerHTML:
// security_test.go forbids it everywhere under web/src, and a page is not worth
// an exception). The document element is replaced, so lang, title and head come
// with it, which is what the page-level rules need to see.
function load(html: string): void {
  const parsed = new DOMParser().parseFromString(html, "text/html");
  document.documentElement.replaceWith(document.importNode(parsed.documentElement, true));
}

afterEach(() => {
  const html = document.createElement("html");
  html.append(document.createElement("head"), document.createElement("body"));
  document.documentElement.replaceWith(html);
});

/** The two page-level facts axe cannot settle without layout. */
function structure(): { mains: number; h1s: number } {
  return { mains: document.querySelectorAll("main").length, h1s: document.querySelectorAll("h1").length };
}

async function audit(): Promise<{ violations: string[]; passed: string[] }> {
  const result = await axe.run(document, {
    runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"] },
    rules: { "color-contrast": { enabled: false }, "target-size": { enabled: false } },
  });
  return {
    violations: result.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`),
    passed: result.passes.map((p) => p.id),
  };
}

// Each page, and the axe rules that must have PASSED on it (not merely not
// failed): the error page has no form, so no label or button rules.
const BASE_RULES = ["document-title", "html-has-lang", "html-lang-valid", "region", "heading-order", "meta-viewport"];
const PAGES: [string, string, string[]][] = [
  ["the full consent page", full, [...BASE_RULES, "label", "button-name"]],
  ["the read-only consent page", readonly, [...BASE_RULES, "label", "button-name"]],
  ["the error page", errorPage, BASE_RULES],
];

describe("the pages the server sends", () => {
  for (const [name, html, rules] of PAGES) {
    it(`${name} has no axe violations, and the rules that matter really ran`, async () => {
      load(html);
      const { violations, passed } = await audit();
      expect(violations).toEqual([]);
      // A denominator: an empty result is only clean if these were checked.
      for (const rule of rules) {
        expect(passed, rule).toContain(rule);
      }
      // landmark-one-main and page-has-heading-one come back "incomplete" in
      // jsdom (they need layout), so they are asserted on the structure instead.
      expect(structure()).toEqual({ mains: 1, h1s: 1 });
    });
  }

  it("every control on the consent pages is labelled, and the count is not zero", async () => {
    for (const [html, atLeast] of [[full, 20], [readonly, 6]] as const) {
      load(html);
      const controls = [...document.querySelectorAll("input:not([type=hidden]), select, textarea")];
      expect(controls.length).toBeGreaterThanOrEqual(atLeast);
      const { passed } = await audit();
      expect(passed).toContain("label");
    }
  });

  it("announces an error, and only the full page has one", () => {
    load(full);
    expect(document.querySelector('[role="alert"]')?.textContent).toContain("type: send and delete");
    load(readonly);
    expect(document.querySelector('[role="alert"]')).toBeNull();
    load(errorPage);
    expect(document.querySelector('[role="alert"]')).not.toBeNull();
  });

  it("carries no script and pins nothing the policy forbids", () => {
    for (const [, html] of PAGES) {
      expect(html.toLowerCase()).not.toContain("<script");
      expect(/\son[a-z]+=/i.test(html)).toBe(false);
    }
  });
});

describe("the audit can fail (negative controls)", () => {
  const break_ = async (mutate: (h: string) => string): Promise<string> => {
    const mutated = mutate(full);
    expect(mutated).not.toBe(full); // the mutation really changed the page
    load(mutated);
    return (await audit()).violations.join("\n");
  };

  it("an unlabelled input is reported", async () => {
    expect(await break_((h) => h.replace('<label for="rate">Calls per minute</label>', ""))).toContain("label");
  });

  it("a page with no title is reported", async () => {
    expect(await break_((h) => h.replace(/<title>[^<]*<\/title>/, ""))).toContain("document-title");
  });

  it("a page with no language is reported", async () => {
    expect(await break_((h) => h.replace('<html lang="en">', "<html>"))).toContain("html-has-lang");
  });

  it("a page with no main landmark or heading fails the structure check", async () => {
    await break_((h) => h.replace("<main>", "<div>").replace("</main>", "</div>").replace("<h1>", "<p>").replace("</h1>", "</p>"));
    expect(structure()).toEqual({ mains: 0, h1s: 0 });
  });

  it("a nameless button is reported", async () => {
    expect(await break_((h) => h.replace(">Approve</button>", "></button>"))).toContain("button-name");
  });

  it("a checkbox with no text is reported", async () => {
    expect(await break_((h) => h.replace('name="suspend" value="1" checked> Suspend the app automatically if it misbehaves', 'name="suspend" value="1" checked>'))).toContain("label");
  });
});
