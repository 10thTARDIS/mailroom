// SPDX-License-Identifier: AGPL-3.0-only

// NO SHORTCUT SHADOWED BY AN EARLIER CASE LABEL (#624 §1).
//
// Each surface's keyboard handler is one `switch (ev.key) { … }`, and a `case`
// that repeats inside it is a shortcut that can never fire: the FIRST matching
// case in a switch wins, and the second is dead code the compiler says nothing
// about — `noFallthroughCasesInSwitch` (tsconfig.json) catches a case body
// falling into the next one without a break/return, not a repeated label. That
// is exactly how Forward (`f`, #582) went unreachable on the desk and the
// in-tray: both routes already had `case "f": case "s":` for the tray toggle,
// landed earlier in the same switch than Forward's own `case "f":`.
//
// THERE IS NO ESLINT IN THIS REPO (web/package.json has no lint script and no
// eslint dependency, so `no-duplicate-case` is not available). This is the
// substitute: read each route's source as text — the same `?raw` trick
// outbox.test.ts uses to assert something about Sidebar.tsx's source, and for
// the same reason (no @types/node, no jsdom, and going through Vite's own
// transform is what the app does anyway) — and assert no `case "<x>":` label
// repeats inside the keyboard switch.
//
// SCOPED TO THAT ONE SWITCH, DELIBERATELY, not the whole file. DeskRoute.tsx and
// TrayRoute.tsx each have three more `switch (item.kind)` blocks below the
// keyboard one (renderItem, rowAriaOf, itemKey) that legitimately reuse the same
// case words ("row", "heading", "more", …) across DIFFERENT switches — a
// whole-file duplicate check would fail on code that has never been the bug.

import { describe, expect, it } from "vitest";

import deskRouteSource from "./DeskRoute.tsx?raw";
import listRouteSource from "./ListRoute.tsx?raw";
import trayRouteSource from "./TrayRoute.tsx?raw";

/** Comments stripped, the same crude way outbox.test.ts strips Sidebar.tsx's:
 * this assertion is about CODE, and a future comment that happens to quote
 * `case "f":` in prose (this file's own header just did) must not read as a
 * second one. Crude on purpose — it would also blank a comment-like run inside
 * a string literal, which none of these case labels are. */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^[^\n]*\/\/.*$/gm, "");
}

/**
 * The text of the keyboard handler's `switch (ev.key) { … }`, found by brace
 * matching from the marker rather than by a fixed line range, so reordering the
 * file does not break this.
 *
 * Throws rather than returning an empty string when the marker is missing, so a
 * rename of the switch fails this test loudly instead of it quietly asserting
 * nothing — the same reason outboxCount's own source-guard test exists.
 */
function keyboardSwitch(source: string, file: string): string {
  const marker = "switch (ev.key)";
  const at = source.indexOf(marker);
  if (at < 0) throw new Error(`${file}: no "${marker}" found — has the handler moved?`);
  const braceStart = source.indexOf("{", at);
  if (braceStart < 0) throw new Error(`${file}: "${marker}" has no opening brace`);
  let depth = 1;
  let i = braceStart + 1;
  for (; i < source.length && depth > 0; i += 1) {
    if (source[i] === "{") depth += 1;
    else if (source[i] === "}") depth -= 1;
  }
  if (depth !== 0) throw new Error(`${file}: unbalanced braces reading the switch`);
  return source.slice(braceStart, i);
}

/** Every `case "…":` label in one switch, in source order — duplicates
 * included, because the whole point is to see them. */
function caseLabels(block: string): string[] {
  return [...block.matchAll(/case "([^"]*)":/g)].map((m) => m[1] as string);
}

function firstDuplicate(labels: string[]): string | null {
  const seen = new Set<string>();
  for (const label of labels) {
    if (seen.has(label)) return label;
    seen.add(label);
  }
  return null;
}

const ROUTES: [string, string][] = [
  ["DeskRoute.tsx", deskRouteSource],
  ["TrayRoute.tsx", trayRouteSource],
  ["ListRoute.tsx", listRouteSource],
];

describe("no shortcut is shadowed by an earlier case label", () => {
  for (const [file, raw] of ROUTES) {
    it(`${file}'s keyboard switch has no repeated case`, () => {
      const labels = caseLabels(keyboardSwitch(stripComments(raw), file));
      // Sanity: a switch with fewer than a dozen cases means the extraction
      // found the wrong brace pair, not that the handler shrank to nothing.
      expect(labels.length).toBeGreaterThan(10);
      expect(firstDuplicate(labels)).toBeNull();
    });
  }

  it("Forward's `f` is exactly one case, and the tray toggle's `s` is still there", () => {
    // The specific shape #624 §1 fixes, named rather than left to the generic
    // scan above: `f` must appear EXACTLY ONCE (Forward) in every one of these
    // switches, not shared with the tray/star toggle any more, and `s` (that
    // toggle) must still be bound.
    for (const [file, raw] of ROUTES) {
      const labels = caseLabels(keyboardSwitch(stripComments(raw), file));
      expect(labels.filter((l) => l === "f")).toHaveLength(1);
      expect(labels).toContain("s");
    }
  });

  it("the stripper leaves real code alone — otherwise every assertion above is vacuous", () => {
    // Without this, a stripper that blanked everything would satisfy every
    // "no duplicate" assertion above by finding no case labels at all.
    const block = keyboardSwitch(stripComments(deskRouteSource), "DeskRoute.tsx");
    expect(block).toMatch(/case "j":/);
    expect(block).toMatch(/doArchive/);
  });
});
