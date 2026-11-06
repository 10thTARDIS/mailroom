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
//   * an image collapses to its alt text.
//
// No editor import: this runs with the composer, not with the editor chunk.

import { parseIndent } from "./indentValue";

/** Marks an indent space while the text is trimmed (a plain space at the start of
 * the text would be trimmed with the rest); turned into a space at the end. Not a
 * character that occurs in mail, and any that did is removed on the way in. */
const PAD = "";

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
