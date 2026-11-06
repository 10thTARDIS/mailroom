// SPDX-License-Identifier: AGPL-3.0-only

// THE ONLY VALUES AN INLINE STYLE MAY CARRY (ADR-051 beta 2). Colour, highlight,
// font family and font size are the editor's first marks that write a CSS value
// taken from markup the user pasted, so the value is never passed through: it is
// matched against a fixed list (families, sizes) or a canonical form (colours),
// and what comes out is OUR string, never the input. Anything else (`url()`,
// `expression()`, `var()`, `@import`, an escape, a comment, `!important`, a font
// list or a unit that is not ours) yields null, and the caller leaves the mark
// off while the text stays.
//
// Pure functions with no editor import, so the plain-text conversion and the tests
// can use them without pulling the editor into the main bundle. The server's
// sanitiser (internal/api/sanitize_css.go) is still the boundary; this is the
// convenience layer in front of it.

/** The font families the toolbar offers, as the exact CSS strings written out. */
export const FONT_FAMILIES: ReadonlyArray<{ id: string; css: string }> = [
  { id: "sans", css: "Arial, Helvetica, sans-serif" },
  { id: "serif", css: "Georgia, 'Times New Roman', serif" },
  { id: "mono", css: "'Courier New', Courier, monospace" },
  { id: "wide", css: "Verdana, Geneva, sans-serif" },
];

/** The font sizes the toolbar offers ("Normal" is no size at all). */
export const FONT_SIZES: ReadonlyArray<{ id: string; css: string }> = [
  { id: "small", css: "12px" },
  { id: "large", css: "18px" },
  { id: "huge", css: "24px" },
];

/** What a declaration's value must not contain whatever the property: a CSS
 * escape (it could spell `url(` past a text match), a comment, a priority, a
 * function call of the kinds that fetch or look things up, an at-rule, a bracket or a control character. Every
 * accepted form below is a plain token, so this is a second line, not the first. */
const SUSPECT = /[\\\u0000-\u001f\u007f]|\/\*|\*\/|!|@|;|\{|\}|<|>|url\s*\(|expression|var\s*\(/i;

/** `#rgb` or `#rrggbb`, or `rgb(r, g, b)` with integer components 0-255, as
 * `#rrggbb` in lower case; null for everything else (names, `rgba`, `hsl`, `var()`). */
export function normaliseColour(value: unknown): string | null {
  if (typeof value !== "string") return null;
  const v = value.trim();
  if (v.length > 32 || SUSPECT.test(v)) return null;
  let m = /^#([0-9a-f]{3})$/i.exec(v);
  if (m) {
    const [r, g, b] = m[1]!.split("");
    return `#${r}${r}${g}${g}${b}${b}`.toLowerCase();
  }
  m = /^#([0-9a-f]{6})$/i.exec(v);
  if (m) return `#${m[1]}`.toLowerCase();
  m = /^rgb\(\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*(\d{1,3})\s*\)$/i.exec(v);
  if (m) {
    const parts = [m[1], m[2], m[3]].map(Number);
    if (parts.some((n) => n > 255)) return null;
    return `#${parts.map((n) => n.toString(16).padStart(2, "0")).join("")}`;
  }
  return null;
}

/** The family list as a comparison key: quotes made single, space around commas
 * made one, case folded. The comparison is to OUR strings, and the caller returns
 * ours, so the folding cannot let anything through that is not on the list. */
function familyKey(value: string): string {
  return value.replace(/"/g, "'").replace(/\s*,\s*/g, ", ").replace(/\s+/g, " ").trim().toLowerCase();
}

/** The listed font family this value names, as our exact string; null if it is not one. */
export function normaliseFontFamily(value: unknown): string | null {
  if (typeof value !== "string" || value.length > 120 || SUSPECT.test(value)) return null;
  const key = familyKey(value);
  return FONT_FAMILIES.find((f) => familyKey(f.css) === key)?.css ?? null;
}

/** The listed font size this value names, as our exact string; null if it is not one. */
export function normaliseFontSize(value: unknown): string | null {
  if (typeof value !== "string" || value.length > 12 || SUSPECT.test(value)) return null;
  const v = value.trim().toLowerCase();
  return FONT_SIZES.find((s) => s.css === v)?.css ?? null;
}

/** The last declaration of `prop` in an inline style attribute, as its raw value,
 * or null when there is none or it carries a priority. Read from the attribute
 * text and not from CSSOM: a parsed declaration has already been normalised or
 * dropped by the browser, and what a validator should judge is what was written.
 * The last declaration wins, as in CSS; if it is unusable, an earlier one is not
 * consulted. */
export function declaredValue(style: string | null | undefined, prop: string): string | null {
  if (!style || style.length > 4096) return null;
  let found: string | null = null;
  for (const decl of style.split(";")) {
    const i = decl.indexOf(":");
    if (i < 0) continue;
    if (decl.slice(0, i).trim().toLowerCase() !== prop) continue;
    const value = decl.slice(i + 1).trim();
    found = /!\s*important/i.test(value) ? "" : value;
  }
  return found === "" ? null : found;
}

/** The value of `prop` for an element and, when it has none of its own, the
 * enclosing `<span>`s (nested spans are one mark in the schema, so an inner span
 * that sets only the size must not lose the outer one's colour). */
export function spanStyle(el: Element, prop: string): string | null {
  for (let cur: Element | null = el; cur && cur.tagName === "SPAN"; cur = cur.parentElement) {
    const v = declaredValue(cur.getAttribute("style"), prop);
    if (v !== null) return v;
  }
  return null;
}
