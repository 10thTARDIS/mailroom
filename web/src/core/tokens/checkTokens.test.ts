// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the #376 design-token checker. Each one is named after the failure
// mode it exists to catch — see checkTokens.ts's header for why each rule exists
// at all. Fixtures are small and synthetic on purpose: the REAL tree is exercised
// separately (the probe in the issue's verification step, and by `npm run
// check:tokens` itself), so these only have to prove the logic, not the repo.

import { describe, expect, it } from "vitest";

import {
  type FileText,
  checkTokens,
  contrastRatio,
  relativeLuminance,
} from "./checkTokens";

/** A minimal but complete tokens.css: one contrast pair, one root token, both
 *  theme blocks with matching names. Individual tests layer deviations on top by
 *  passing a different tokens.css text where the deviation matters. */
function baseTokensCss(): string {
  return [
    "/* SPDX-License-Identifier: AGPL-3.0-only */",
    "",
    "/* @contrast fg/bg */",
    "",
    ":root {",
    "  --radius: 8px;",
    "}",
    "",
    'html[data-theme="light"] {',
    "  --fg: #16191d;",
    "  --bg: #ffffff;",
    "}",
    "",
    'html[data-theme="dark"] {',
    "  --fg: #e8eaed;",
    "  --bg: #16191d;",
    "}",
    "",
  ].join("\n");
}

function tokensFile(text = baseTokensCss()): FileText {
  return { path: "src/app/tokens.css", text };
}

describe("checkTokens: used but undefined", () => {
  it("is a finding at the file:line of the use", () => {
    const other: FileText = { path: "src/app/styles.css", text: ".a { color: var(--missing); }\n" };
    const result = checkTokens(tokensFile(), [other]);
    expect(result.findings).toContain(
      "src/app/styles.css:1: var(--missing) is used but never defined in tokens.css (#376)",
    );
  });

  it("does not flag a component-scoped local property that IS defined, just not in tokens.css", () => {
    // The --tab-strip-h shape: declared and used entirely inside one non-token
    // selector in an ordinary stylesheet. This is the negative control for the
    // rule above — without it, moving styles.css's scoped variable pattern into
    // this checker would make every build red on day one.
    const other: FileText = {
      path: "src/app/styles.css",
      text: [".list-pane {", "  --tab-strip-h: 2.4rem;", "}", ".tab-strip { min-height: var(--tab-strip-h); }", ""].join(
        "\n",
      ),
    };
    const result = checkTokens(tokensFile(), [other]);
    expect(result.findings).toEqual([]);
  });
});

describe("checkTokens: definitions outside tokens.css", () => {
  it("flags a :root/theme/density redefinition in another file, at its line", () => {
    const other: FileText = {
      path: "src/app/styles.css",
      text: [":root {", "  --accent: #ff0000;", "}", ""].join("\n"),
    };
    const result = checkTokens(tokensFile(), [other]);
    expect(
      result.findings.some(
        (f) => f.startsWith("src/app/styles.css:2:") && f.includes("--accent") && f.includes("outside tokens.css"),
      ),
    ).toBe(true);
  });

  it("does NOT flag a declaration scoped to an ordinary component selector", () => {
    const other: FileText = {
      path: "src/app/styles.css",
      text: [".list-pane {", "  --tab-strip-h: 2.4rem;", "}", ""].join("\n"),
    };
    const result = checkTokens(tokensFile(), [other]);
    expect(result.findings.some((f) => f.includes("--tab-strip-h"))).toBe(false);
  });

  it("does NOT flag a theme selector nested inside @media as a top-level redefinition", () => {
    // The real styles.css shape: `@media (prefers-contrast: more) { html[data-
    // theme="light"] { --border: …; } }` strengthens an already-defined token
    // for a media feature; it is not a second definition of --border.
    const other: FileText = {
      path: "src/app/styles.css",
      text: [
        "@media (prefers-contrast: more) {",
        '  html[data-theme="light"] { --border: #000000; }',
        '  html[data-theme="dark"] { --border: #ffffff; }',
        "}",
        "",
      ].join("\n"),
    };
    const result = checkTokens(tokensFile(), [other]);
    expect(result.findings.some((f) => f.includes("--border"))).toBe(false);
  });
});

describe("checkTokens: theme symmetry", () => {
  it("flags a token present in one theme block but missing from the other", () => {
    const text = [
      "/* @contrast fg/bg */",
      ":root {}",
      'html[data-theme="light"] {',
      "  --fg: #000000;",
      "  --bg: #ffffff;",
      "  --only-light: #123456;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #ffffff;",
      "  --bg: #000000;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(
      result.findings.some((f) => f.includes("--only-light") && f.includes("missing from the dark theme block")),
    ).toBe(true);
  });
});

describe("checkTokens: contrast pairs", () => {
  it("flags a pair below 4.5:1, naming the failing theme", () => {
    const text = [
      "/* @contrast fg/bg */",
      ":root {}",
      // #777777 on #ffffff is ~4.48:1 — just under AA.
      'html[data-theme="light"] {',
      "  --fg: #777777;",
      "  --bg: #ffffff;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #ffffff;",
      "  --bg: #000000;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(result.findings.some((f) => f.includes("fg/bg") && f.includes("light") && f.includes("below"))).toBe(true);
    // The dark pair (21:1) must NOT also be flagged — this is the pair's
    // negative control: a checker that flagged every pair regardless of theme
    // would still pass this test's first assertion by accident.
    expect(result.findings.some((f) => f.includes("fg/bg") && f.includes("dark") && f.includes("below"))).toBe(false);
  });

  it("does not flag a pair listed on @contrast-known-low, in the theme it names", () => {
    const text = [
      "/* @contrast fg/bg */",
      "/* @contrast-known-low light fg/bg 4.48 */",
      ":root {}",
      'html[data-theme="light"] {',
      "  --fg: #777777;",
      "  --bg: #ffffff;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #ffffff;",
      "  --bg: #000000;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(result.findings).toEqual([]);
  });

  it("flags a non-hex value in a contrast pair as an error, not a silent skip", () => {
    const text = [
      "/* @contrast fg/bg */",
      ":root {}",
      'html[data-theme="light"] {',
      "  --fg: rgb(0, 0, 0);",
      "  --bg: #ffffff;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #ffffff;",
      "  --bg: #000000;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(result.findings.some((f) => f.includes("not a plain hex colour"))).toBe(true);
  });
});

describe("checkTokens: known-low staleness", () => {
  it("flags a known-low pair that now measures past 4.5:1", () => {
    const text = [
      "/* @contrast fg/bg */",
      "/* @contrast-known-low light fg/bg 4.48 */",
      ":root {}",
      // #767676 on #ffffff is ~4.54:1 — now passes AA, so the debt line is stale.
      'html[data-theme="light"] {',
      "  --fg: #767676;",
      "  --bg: #ffffff;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #ffffff;",
      "  --bg: #000000;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(result.findings.some((f) => f.includes("@contrast-known-low") && f.includes("now measures"))).toBe(true);
  });
});

describe("checkTokens: contrast math against known values", () => {
  it("black on white is 21:1", () => {
    expect(contrastRatio("#000000", "#ffffff")).toBeCloseTo(21, 1);
  });

  it("#777777 on white is just under AA (~4.48)", () => {
    const ratio = contrastRatio("#777777", "#ffffff")!;
    expect(ratio).toBeCloseTo(4.48, 1);
    expect(ratio).toBeLessThan(4.5);
  });

  it("#767676 on white is just over AA (~4.54)", () => {
    const ratio = contrastRatio("#767676", "#ffffff")!;
    expect(ratio).toBeCloseTo(4.54, 1);
    expect(ratio).toBeGreaterThan(4.5);
  });

  it("relativeLuminance rejects a non-hex value instead of guessing", () => {
    expect(relativeLuminance("rgb(0,0,0)")).toBeNull();
  });
});

describe("checkTokens: unused tokens are notes, not findings", () => {
  it("a defined-but-unused token produces a note and no finding", () => {
    const text = [
      "/* @contrast fg/bg */",
      ":root {",
      "  --unused-example: 1px;",
      "}",
      'html[data-theme="light"] {',
      "  --fg: #16191d;",
      "  --bg: #ffffff;",
      "}",
      'html[data-theme="dark"] {',
      "  --fg: #e8eaed;",
      "  --bg: #16191d;",
      "}",
      "",
    ].join("\n");
    const result = checkTokens(tokensFile(text), []);
    expect(result.findings).toEqual([]);
    expect(result.notes).toContain("--unused-example is defined but not used anywhere (#376)");
  });

  it("a used token is neither an unused note nor a finding", () => {
    const other: FileText = { path: "src/app/styles.css", text: ":root-user { color: var(--fg); }\n" };
    const result = checkTokens(tokensFile(), [other]);
    expect(result.findings).toEqual([]);
    expect(result.notes.some((n) => n.includes("--fg"))).toBe(false);
  });
});

describe("checkTokens: summary", () => {
  it("counts defined, used, pairs-per-theme and notes", () => {
    const other: FileText = { path: "src/app/styles.css", text: ".a { color: var(--fg); }\n" };
    const result = checkTokens(tokensFile(), [other]);
    // base fixture defines --radius, --fg, --bg = 3 tokens; only --fg is used.
    expect(result.summary.definedCount).toBe(3);
    expect(result.summary.usedCount).toBe(1);
    expect(result.summary.pairsPerTheme).toBe(1);
    expect(result.summary.notesCount).toBe(2);
    expect(result.summaryLine).toContain("3 defined");
    expect(result.summaryLine).toContain("1 used");
  });
});
