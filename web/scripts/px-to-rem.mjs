// SPDX-License-Identifier: AGPL-3.0-only
//
// THE REM SWEEP (mailroom#514). One mechanical pass that turns the app's
// hard-coded pixel type — and the box metrics that exist only to FIT that type
// — into `rem`/`em` at the historical 14px root, so that
// `html[data-text-size]` (app/prefs.ts applyTextSize) can drive the whole app
// from one declaration instead of from 147 of them.
//
// IT IS KEPT rather than run and thrown away for the reason every conversion
// script should be: the numbers below are the ONLY evidence that
// `0.9286rem` was once `13px`, and a reviewer who wants to check the
// arithmetic should not have to reverse it out of a diff.
//
// ⚠ IT IS A RECORD OF ONE CONVERSION, NOT AN IDEMPOTENT TOOL. Run again on the
// converted tree it THROWS on the first box metric it cannot find, and that is
// the right failure: every entry in the second table below declares how many
// times it must match, so a rule that silently stopped matching would be a
// conversion that silently did less than it says. Do not "fix" it by making the
// counts optional.
//
// WHAT IT DOES NOT DO, and this is the part that needed judgement rather than
// a regex:
//
//   * It leaves every OTHER pixel value alone. A rail width, a pane split, the
//     24% sender column, a colour swatch, a drag grip, a 32px tap target and
//     every border/shadow/radius are about POINTERS, GEOMETRY or GRAPHICS, not
//     about text — scaling them with the type would make a text-size control
//     into a zoom control, which is a different feature and a worse one.
//   * The second table below is therefore an EXPLICIT ALLOWLIST of the box
//     metrics that do exist to fit text (the row height and row padding
//     tokens, button and toolbar minimum heights, the composer's text
//     surfaces, the two unread dots that sit in a line box beside a name).
//     Every entry is a literal string with an expected occurrence count, so a
//     rule that stops matching is an error rather than a silent skip.
//   * `line-height` was already unitless everywhere in this tree (measured:
//     eleven declarations, none in px), so the sweep has nothing to do there.
//
// Usage (from web/):  node scripts/px-to-rem.mjs

import { readFileSync, writeFileSync } from "node:fs";

/** The root font size every one of these numbers was authored against, and the
 *  size `data-text-size="m"` still sets — so this conversion is a no-op on
 *  screen, by construction. */
const BASE = 14;

const FILES = [
  "src/app/tokens.css",
  "src/app/styles.css",
  "src/app/desk.css",
  "src/app/compose.css",
  "src/app/setup.css",
  "src/app/viewer.css",
  "src/app/settings.css",
];

/** N px as a rem/em string at the 14px base: four decimals, trailing zeros
 *  trimmed, so 14px is `1rem` rather than `1.0000rem` and 13px is `0.9286rem`
 *  (0.02px of error at the largest step — far inside a device pixel). */
function scale(px, unit = "rem") {
  const n = Number(px) / BASE;
  const s = n.toFixed(4).replace(/0+$/, "").replace(/\.$/, "");
  return s + unit;
}

// ---------------------------------------------------------------------------
// 1. Every `font-size: Npx`, mechanically.
// ---------------------------------------------------------------------------
const FONT_SIZE_RE = /font-size:\s*([0-9.]+)px/g;

// ---------------------------------------------------------------------------
// 2. The box metrics that exist to fit text — named one at a time.
// ---------------------------------------------------------------------------
//
// [file, literal to replace, replacement, how many times it must occur]
const BOX_METRICS = [
  // The desk's row height and the ordinary list row's padding, per density.
  // These are THE reason the sweep exists: a row whose height stayed in pixels
  // would clip its own text at the two largest steps, and the virtual list
  // would place every row after it over the top of the one before.
  // ⚠ THE `:root` PAIR IS MATCHED WITH ITS TWO-SPACE INDENTATION, because the
  // identical
  // declaration also appears (on one line, single-spaced) in the cozy density
  // block below — `:root` deliberately restates the cozy values so anything
  // that reads a token before `applyDensity` runs sees a real number rather
  // than `initial` (tokens.css says so at both). Two entries, two literals.
  ["src/app/tokens.css", "  --row-pad: 9px;", `  --row-pad: ${scale(9)};`, 1],
  ["src/app/tokens.css", "  --desk-row-h: 56px;", `  --desk-row-h: ${scale(56)};`, 1],
  [
    "src/app/tokens.css",
    'html[data-density="comfortable"] { --row-pad: 14px; --desk-row-h: 64px; }',
    `html[data-density="comfortable"] { --row-pad: ${scale(14)}; --desk-row-h: ${scale(64)}; }`,
    1,
  ],
  [
    "src/app/tokens.css",
    'html[data-density="cozy"] { --row-pad: 9px; --desk-row-h: 56px; }',
    `html[data-density="cozy"] { --row-pad: ${scale(9)}; --desk-row-h: ${scale(56)}; }`,
    1,
  ],
  [
    "src/app/tokens.css",
    'html[data-density="compact"] { --row-pad: 5px; --desk-row-h: 46px; }',
    `html[data-density="compact"] { --row-pad: ${scale(5)}; --desk-row-h: ${scale(46)}; }`,
    1,
  ],
  // The phone block's own three, which override the same token (desk.css).
  [
    "src/app/desk.css",
    'html[data-density="comfortable"] { --desk-row-h: 76px; }',
    `html[data-density="comfortable"] { --desk-row-h: ${scale(76)}; }`,
    1,
  ],
  [
    "src/app/desk.css",
    'html[data-density="cozy"] { --desk-row-h: 68px; }',
    `html[data-density="cozy"] { --desk-row-h: ${scale(68)}; }`,
    1,
  ],
  [
    "src/app/desk.css",
    'html[data-density="compact"] { --desk-row-h: 58px; }',
    `html[data-density="compact"] { --desk-row-h: ${scale(58)}; }`,
    1,
  ],
  // Controls whose height is set by the WORD inside them: the rail's items,
  // every .button and .icon-button, the composer's toolbar buttons, its body
  // surface and its recipient-chip field. A 34px button around 18px text is a
  // button with clipped descenders.
  ["src/app/styles.css", "  min-height: 36px;", `  min-height: ${scale(36)};`, 1],
  ["src/app/styles.css", "  min-height: 28px;", `  min-height: ${scale(28)};`, 1],
  ["src/app/styles.css", "  min-height: 180px;", `  min-height: ${scale(180)};`, 1],
  ["src/app/styles.css", "  min-height: 34px;", `  min-height: ${scale(34)};`, 2],
  ["src/app/styles.css", "  min-width: 34px;", `  min-width: ${scale(34)};`, 1],
  ["src/app/compose.css", "  min-height: 34px;", `  min-height: ${scale(34)};`, 1],
  // The unread dot, in both lists. It is not a graphic sitting beside the row —
  // it is drawn INSIDE the sender's line box as a `::before`, so at the largest
  // step a fixed 8px dot with a fixed 7px gap reads as debris rather than as
  // part of the name. `em`, not `rem`: it belongs to the text it marks.
  [
    "src/app/styles.css",
    "  width: 8px;\n  height: 8px;\n  margin-right: 7px;",
    `  width: ${scale(8, "em")};\n  height: ${scale(8, "em")};\n  margin-right: ${scale(7, "em")};`,
    1,
  ],
  [
    "src/app/desk.css",
    "  width: 8px;\n  height: 8px;\n  margin-right: 7px;",
    `  width: ${scale(8, "em")};\n  height: ${scale(8, "em")};\n  margin-right: ${scale(7, "em")};`,
    1,
  ],
];

let boxes = 0;
const perFile = new Map();

for (const file of FILES) {
  let text = readFileSync(file, "utf8");
  let n = 0;
  text = text.replace(FONT_SIZE_RE, (_m, px) => {
    n++;
    return `font-size: ${scale(px)}`;
  });
  for (const [f, from, to, want] of BOX_METRICS) {
    if (f !== file) continue;
    const got = text.split(from).length - 1;
    if (got !== want) {
      throw new Error(`${file}: expected ${want} occurrence(s) of ${JSON.stringify(from)}, found ${got}`);
    }
    text = text.split(from).join(to);
    boxes += got;
    n += got;
  }
  writeFileSync(file, text);
  perFile.set(file, n);
}

for (const [file, n] of perFile) console.log(`${file}: ${n} declarations converted`);
console.log(`total: ${[...perFile.values()].reduce((a, b) => a + b, 0)} (${boxes} of them box metrics)`);
