// SPDX-License-Identifier: AGPL-3.0-only

// HTML to plain text, for the multipart/alternative text part and for the
// rich-to-plain toggle (ADR-051 §3: "rich to plain degrades each format
// sensibly"). A readable rendering, not a perfect one:
//
//   * a block quote's lines start with "> ";
//   * code is kept verbatim, line breaks and spaces included;
//   * a list item gets a bullet, a nested one is indented under its parent;
//   * a paragraph's indent (margin-left, ./indentValue.ts) becomes leading spaces,
//     two per level;
//   * strike-through, colour, highlight, font and size are dropped, and
//     subscript and superscript are kept as their text;
//   * an image collapses to its alt text;
//   * a table becomes aligned text (tableToText below).
//
// No editor import: this runs with the composer, not with the editor chunk.

import { parseIndent } from "./indentValue";

/** Marks an indent space while the text is trimmed (a plain space at the start of
 * the text would be trimmed with the rest); turned into a space at the end. Not a
 * character that occurs in mail, and any that did is removed on the way in. */
const PAD = "";
const PAD_RE = new RegExp(PAD, "g");

/** The widest a table column is drawn in plain text; a longer cell is cut and
 * ends in "…". So a table stays readable instead of one long cell pushing every
 * other column off the line. */
export const TABLE_COLUMN_MAX = 40;

/** Width of a cell in plain text: CODE POINTS, not UTF-16 units, so "é" and an
 * emoji each count as one. East Asian wide characters (two columns in a
 * monospace font) and combining marks are NOT measured specially: a column with
 * them may look a little ragged, never wrong. */
const width = (s: string): number => Array.from(s).length;

function truncate(s: string, max: number): string {
  const cps = Array.from(s);
  return cps.length <= max ? s : `${cps.slice(0, max - 1).join("")}…`;
}

/** A table as aligned text. Each cell's text is its inner text with every line
 * break and run of spaces turned into one space, cut to TABLE_COLUMN_MAX. Columns
 * are padded to their widest cell and separated by two spaces; the line ends where
 * the last cell's text does. A header row (every cell a `th`) is followed by a
 * line of dashes. colspan and rowspan are not drawn: a spanned cell is one cell. */
function tableToText(table: HTMLElement): string {
  const rows: string[][] = [];
  let headerRow = -1;
  table.querySelectorAll("tr").forEach((tr) => {
    // Only this table's own rows; a nested table is flattened into its cell's text.
    if (tr.closest("table") !== table) return;
    const cells = Array.from(tr.children).filter((c) => /^(td|th)$/i.test(c.tagName));
    if (cells.length === 0) return;
    if (headerRow < 0 && rows.length === 0 && cells.every((c) => c.tagName.toLowerCase() === "th")) headerRow = 0;
    rows.push(cells.map((c) => truncate(walkText(c).replace(PAD_RE, " ").replace(/\s+/g, " ").trim(), TABLE_COLUMN_MAX)));
  });
  if (rows.length === 0) return "";
  const cols = Math.max(...rows.map((r) => r.length));
  const widths = Array.from({ length: cols }, (_, i) => Math.max(1, ...rows.map((r) => width(r[i] ?? ""))));
  // Padding is PAD, not a space, so it survives the trim of the whole text (a
  // table that starts with an empty cell keeps its alignment).
  const line = (cells: string[]): string =>
    cells
      .map((c, i) => `${c}${PAD.repeat(widths[i]! - width(c))}`)
      .join(PAD.repeat(2))
      .replace(new RegExp(`${PAD}+$`), "");
  const out: string[] = [];
  rows.forEach((r, i) => {
    out.push(line(Array.from({ length: cols }, (_, c) => r[c] ?? "")));
    if (i === 0 && headerRow === 0) out.push(line(widths.map((w) => "-".repeat(w))));
  });
  return `${out.join("\n")}\n`;
}

export function htmlToText(html: string): string {
  // DOMParser builds an inert document (no scripts run, nothing loads), which is
  // all this needs: walk it for text. (ADR-051 §4; this was a <template> before.)
  const doc = new DOMParser().parseFromString(html, "text/html");
  return walkText(doc.body)
    .replace(/[ \t]+\n/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim()
    .replace(new RegExp(PAD, "g"), " ");
}

function prefixLines(text: string, prefix: string): string {
  return text
    .split("\n")
    .map((line) => (line === "" ? line : `${prefix}${line}`))
    .join("\n");
}

function walkText(node: Node): string {
  let out = "";
  node.childNodes.forEach((child) => {
    if (child.nodeType === Node.TEXT_NODE) {
      out += (child.textContent ?? "").replace(//g, "").replace(/\s+/g, " ");
      return;
    }
    if (child.nodeType !== Node.ELEMENT_NODE) return;
    const el = child as HTMLElement;
    const tag = el.tagName.toLowerCase();
    if (tag === "br") {
      out += "\n";
      return;
    }
    if (tag === "img") {
      const alt = el.getAttribute("alt");
      out += alt ? `[${alt}]` : "[image]";
      return;
    }
    if (tag === "table") {
      if (out !== "" && !out.endsWith("\n")) out += "\n";
      out += tableToText(el);
      return;
    }
    if (tag === "pre") {
      // Verbatim: the text exactly as written, not whitespace-collapsed.
      out += `${(el.textContent ?? "").replace(//g, "").replace(/\n$/, "")}\n`;
      return;
    }
    let inner = walkText(el);
    const level = /^(p|div|h[1-6]|blockquote)$/.test(tag) ? parseIndent(el.getAttribute("style")) : 0;
    if (level > 0) inner = prefixLines(inner, PAD.repeat(level * 2));
    if (tag === "blockquote") {
      out += `${inner
        .replace(/\n+$/, "")
        .split("\n")
        .map((line) => `> ${line}`)
        .join("\n")}\n`;
    } else if (tag === "li") {
      // The item's own lines are indented under its bullet, so a nested list reads
      // as nested.
      const [first = "", ...rest] = inner.replace(/\n+$/, "").split("\n");
      out += `• ${[first, ...rest.map((l) => (l === "" ? l : `  ${l}`))].join("\n")}\n`;
    } else if (/^(p|div|h[1-6]|ul|ol|tr|table|section|article|header|footer|figure)$/.test(tag)) {
      out += `${inner}\n`;
    } else {
      out += inner;
    }
  });
  return out;
}
