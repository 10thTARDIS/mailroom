// SPDX-License-Identifier: AGPL-3.0-only

// THE FORMATTED QUOTE (ADR-051 §7, #92): a reply or forward quotes an HTML
// original WITH its formatting.
//
// WHAT GOES IN is never the raw part. It is the HTML body value as the JMAP API
// returns it, which the server's reader sanitiser (internal/api/body.go
// bodyValues) has already been through: active content is gone, CSS is
// allowlisted, and remote pictures are PARKED (`data-mailroom-remote-src`,
// `data-mailroom-blocked`, or a `/jmap/proxy/` path). This module is the second of
// three layers (the editor's schema on seed is the third, SanitizeOutboundHTML on
// save and send the last), and it does only what the other two cannot:
//
//   * decide which pictures may stay (none from the web, ever; a `cid:` one only
//     when the reply can carry its bytes itself);
//   * flatten tables that lay a message out rather than hold data;
//   * keep the quote to a size budget, at a whole block.
//
// ALL OF IT RUNS ON A PARSED DOM, NEVER ON STRINGS. `DOMParser` builds an inert
// document: nothing loads and no handler runs while it is read or changed. The
// result is read back through `XMLSerializer` (the web scan forbids innerHTML and
// outerHTML everywhere), and the editor then parses that string into its own
// schema, which drops everything it does not define.
//
// NOTHING HERE WIDENS A SANITISER. It only removes.

import type { BlobId } from "../../../core/jmap/types";
import { isInlineImageSrc } from "../editor/uri";

/** The most original TEXT a quote keeps, counted in code points. A quote is a
 * convenience; a 400-screen newsletter is not one, and the composer has to stay
 * quick to open and to save. Cut at a whole block, then a note says so. */
export const QUOTE_TEXT_BUDGET = 50_000;

/** The most picture bytes one reply carries as its own inline attachments. The
 * server's own limit for a whole message is larger; this keeps a quote from using
 * it up before the user has written anything. */
export const QUOTE_PICTURE_BYTE_CAP = 10 * 1024 * 1024;

/** The most elements a quote keeps. Text is not the only cost: a body of empty or
 * picture-only elements carries no text and would otherwise be seeded whole, which
 * is a long hang on Reply (the security review of #92). Cut like the text budget:
 * at a whole block, with the note. */
export const QUOTE_NODE_BUDGET = 5_000;

/** The most pictures a quote keeps; later ones get the picture note. */
export const QUOTE_PICTURE_COUNT_CAP = 50;

/** A part of the original, as the quote needs to know it (an `EmailBodyPart`). */
export interface QuotePart {
  partId: string | null;
  blobId: BlobId | null;
  type: string;
  name: string | null;
  size: number;
  cid: string | null;
}

export interface QuoteInput {
  /** The server-sanitised HTML of the original. */
  html: string;
  /** The server cut the body value short. */
  truncated: boolean;
  /** The original's attachments (where an inline picture's part is listed). */
  parts: readonly QuotePart[];
  /** Whether the server refuses to attach this part right now (quarantine). */
  withheld: (partId: string | null) => boolean;
  /** False when the reply cannot carry any picture by reference. */
  canCarry: boolean;
  /** Our own words, in the composer's language. */
  notes: { picture: string; rest: string };
}

/** An original's picture the reply carries as its own inline attachment. */
export interface CarriedPicture {
  blobId: BlobId;
  cid: string;
  name: string;
  type: string;
  partId: string | null;
}

export interface Quote {
  /** The quoted blocks, as HTML for the editor to parse. "" when the original
   * leaves nothing to quote (the caller then falls back to the plain quote). */
  html: string;
  carried: CarriedPicture[];
}

/** The path the server's image proxy parks a remote picture under. */
export const PROXY_PATH_MARKER = "/jmap/proxy/";

/** Attributes the reader sanitiser parks a picture's remote address under. Any one
 * of them on an <img> means the picture came from the web. */
const PARKED_ATTRS = [
  "data-mailroom-remote-src",
  "data-mailroom-remote-srcset",
  "data-mailroom-blocked",
  "data-mailroom-pixel-src",
  "data-mailroom-pixel-srcset",
] as const;

/** Elements that start a new block. Everything else is inline. */
const BLOCK_TAGS = new Set([
  "address", "article", "aside", "blockquote", "caption", "center", "dd", "details", "dl", "dt",
  "fieldset", "figcaption", "figure", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header",
  "hr", "li", "main", "nav", "ol", "p", "pre", "section", "table", "tbody", "td", "tfoot", "th",
  "thead", "tr", "ul", "div",
]);

/** Containers a size cut may look inside: they hold blocks and carry no text of
 * their own, so keeping their leading children is still keeping whole blocks. */
const WRAPPER_TAGS = new Set(["div", "section", "article", "main", "center", "header", "footer"]);

const MAX_COLUMNS = 20;

const ELEMENT = 1;
const TEXT = 3;

function isElement(n: Node): n is Element {
  return n.nodeType === ELEMENT;
}

function tag(n: Node): string {
  return isElement(n) ? n.localName.toLowerCase() : "";
}

function isBlock(n: Node): boolean {
  return BLOCK_TAGS.has(tag(n));
}

/** The string as a browser's URL parser would start from it: no tab, LF or CR, no
 * leading or trailing controls and spaces. */
function urlView(raw: string): string {
  return raw.replace(/[\t\n\r]/g, "").trim();
}

/** The length of the text in code points (a surrogate pair counts once). */
function codePoints(s: string): number {
  let n = 0;
  for (const _ of s) {
    void _;
    n += 1;
  }
  return n;
}

function textLength(n: Node): number {
  return codePoints(n.textContent ?? "");
}

/** The content-id a `cid:` source names, exactly as the reader matches it
 * (inlineImages.ts normalizeCid, and the server): brackets off, NOT percent-decoded.
 * Decoding here once let a reply carry a picture the reading pane could not show. */
function cidOf(src: string): string {
  return urlView(src).slice(4).replace(/^<|>$/g, "");
}

function plainCid(cid: string | null): string {
  return (cid ?? "").trim().replace(/^<|>$/g, "").trim();
}

/** The key a Content-ID is matched on: RFC 2392 names it without the brackets, and
 * the reader (./../inlineImages.ts normalizeCid) compares it case-insensitively, so
 * the quote and the reading pane cannot disagree about which part a `cid:` names. */
function cidKey(cid: string | null): string {
  return plainCid(cid).toLowerCase();
}

interface Ctx {
  doc: Document;
  input: QuoteInput;
  carried: CarriedPicture[];
  carriedBytes: number;
  /** Pictures kept so far, against QUOTE_PICTURE_COUNT_CAP. */
  kept: number;
  notes: Set<Node>;
}

/** A hidden beacon: it carries a size of one pixel or less. Dropped without a
 * note, because the reader sees nothing there to say anything about. */
function isPixel(img: Element): boolean {
  const small = (v: string | null): boolean => {
    if (v === null) return false;
    const n = Number.parseInt(v, 10);
    return Number.isFinite(n) && n <= 1;
  };
  return small(img.getAttribute("width")) || small(img.getAttribute("height"));
}

function addNote(ctx: Ctx, img: Element): void {
  // Several removed pictures in a row say it once.
  let prev: Node | null = img.previousSibling;
  while (prev && prev.nodeType === TEXT && (prev.textContent ?? "").trim() === "" && !ctx.notes.has(prev)) {
    prev = prev.previousSibling;
  }
  if (prev && ctx.notes.has(prev)) {
    img.remove();
    return;
  }
  const note = ctx.doc.createTextNode(ctx.input.notes.picture);
  ctx.notes.add(note);
  img.replaceWith(note);
}

/** Decides one picture: kept (as a `cid:` one the reply carries), or removed. */
function handleImage(ctx: Ctx, img: Element): void {
  // From the web, parked by the reader: never revived, whatever else it has.
  if (PARKED_ATTRS.some((a) => img.hasAttribute(a)) || img.hasAttribute("srcset")) {
    if (img.hasAttribute("data-mailroom-pixel-src") || isPixel(img)) img.remove();
    else addNote(ctx, img);
    return;
  }
  const src = img.getAttribute("src") ?? "";
  const view = urlView(src).toLowerCase();
  if (view.includes(PROXY_PATH_MARKER)) {
    if (isPixel(img)) img.remove();
    else addNote(ctx, img);
    return;
  }
  if (ctx.kept >= QUOTE_PICTURE_COUNT_CAP) {
    if (isPixel(img)) img.remove();
    else addNote(ctx, img);
    return;
  }
  if (view.startsWith("cid:")) {
    const kept = keepCid(ctx, img, cidOf(src));
    if (kept) ctx.kept += 1;
    if (!kept) {
      if (isPixel(img)) img.remove();
      else addNote(ctx, img);
    }
    return;
  }
  // Inline raster bytes (data:image/*) are the picture itself, not a fetch; they
  // count against the same byte cap. Anything else is a fetch or a link.
  if (isInlineImageSrc(src)) {
    const bytes = src.length;
    if (ctx.carriedBytes + bytes <= QUOTE_PICTURE_BYTE_CAP) {
      ctx.carriedBytes += bytes;
      ctx.kept += 1;
      stripImage(img);
      return;
    }
  }
  if (isPixel(img)) img.remove();
  else addNote(ctx, img);
}

/** Keeps only what an inline picture may say: where it is and what it shows. */
function stripImage(img: Element): void {
  for (const name of img.getAttributeNames()) {
    if (name !== "src" && name !== "alt") img.removeAttribute(name);
  }
}

function keepCid(ctx: Ctx, img: Element, cid: string): boolean {
  if (!ctx.input.canCarry || cid === "") return false;
  const key = cidKey(cid);
  const part = ctx.input.parts.find((p) => cidKey(p.cid) === key && p.blobId);
  if (!part || !part.blobId) return false;
  const type = part.type.toLowerCase();
  if (!type.startsWith("image/") || type.startsWith("image/svg")) return false;
  if (ctx.input.withheld(part.partId)) return false;
  // The reply names the picture by the part's own Content-ID.
  const own = plainCid(part.cid);
  if (!ctx.carried.some((c) => cidKey(c.cid) === key)) {
    if (ctx.carriedBytes + part.size > QUOTE_PICTURE_BYTE_CAP) return false;
    ctx.carriedBytes += part.size;
    ctx.carried.push({
      blobId: part.blobId,
      cid: own,
      name: part.name ?? "image",
      type: part.type,
      partId: part.partId,
    });
  }
  stripImage(img);
  img.setAttribute("src", `cid:${own}`);
  return true;
}

/** The last value an inline style gives a property, lowercased, or null. */
function styleValue(el: Element, prop: string): string | null {
  const style = el.getAttribute("style");
  if (!style) return null;
  let found: string | null = null;
  for (const decl of style.split(";")) {
    const i = decl.indexOf(":");
    if (i < 0) continue;
    if (decl.slice(0, i).trim().toLowerCase() === prop) found = decl.slice(i + 1).replace(/!\s*important/i, "").trim().toLowerCase();
  }
  return found;
}

/** Whether the reading pane would not show this element: the sender hid it. The
 * editor keeps none of these styles, so without this the quote would UNHIDE text
 * the recipient never saw and attribute it to the sender. */
function isHidden(el: Element): boolean {
  if (el.hasAttribute("hidden")) return true;
  if (styleValue(el, "display") === "none") return true;
  const vis = styleValue(el, "visibility");
  if (vis === "hidden" || vis === "collapse") return true;
  const opacity = styleValue(el, "opacity");
  if (opacity !== null && Number.parseFloat(opacity) === 0) return true;
  const size = styleValue(el, "font-size");
  if (size !== null && /^0*\.?0*(px|pt|em|rem|%)?$/.test(size) && Number.parseFloat(size || "0") === 0) return true;
  return false;
}

/** A colour's relative luminance (WCAG), for `#rgb`, `#rrggbb` and `rgb(r,g,b)`;
 * null for anything else (the editor keeps no other colour form). */
function luminance(value: string): number | null {
  let rgb: number[] | null = null;
  let m = /^#([0-9a-f])([0-9a-f])([0-9a-f])$/i.exec(value);
  if (m) rgb = [m[1]!, m[2]!, m[3]!].map((h) => Number.parseInt(h + h, 16));
  m = rgb ? null : /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(value);
  if (m) rgb = [m[1]!, m[2]!, m[3]!].map((h) => Number.parseInt(h, 16));
  m = rgb ? null : /^rgba?\(\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*(\d{1,3})\s*(?:,[^)]*)?\)$/i.exec(value);
  if (m) rgb = [m[1]!, m[2]!, m[3]!].map(Number);
  if (!rgb || rgb.some((n) => n > 255)) return null;
  const lin = rgb.map((c) => {
    const v = c / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * lin[0]! + 0.7152 * lin[1]! + 0.0722 * lin[2]!;
}

/** Text colours at or above this luminance are dropped from a quote: near-white
 * text on a mail's usual white page is invisible, so it would travel unseen in the
 * reply. The text stays, in the default colour. */
export const QUOTE_NEAR_WHITE = 0.85;

/** Removes what the sender hid, and near-white text colour. */
function handleHidden(root: Element): void {
  for (const el of Array.from(root.querySelectorAll("*"))) {
    if (!root.contains(el)) continue;
    if (isHidden(el)) {
      el.remove();
      continue;
    }
    const colour = styleValue(el, "color");
    const lum = colour === null ? null : luminance(colour);
    if (lum !== null && lum >= QUOTE_NEAR_WHITE) {
      const kept = (el.getAttribute("style") ?? "")
        .split(";")
        .filter((d) => d.trim() !== "" && d.slice(0, d.indexOf(":")).trim().toLowerCase() !== "color");
      if (kept.length) el.setAttribute("style", kept.join(";"));
      else el.removeAttribute("style");
    }
  }
}

/** Whether a table reads as data (§7): no nested table, not marked presentational,
 * some row with more than one cell, and no more than 20 columns. */
export function isDataTable(table: Element): boolean {
  if (table.querySelector("table")) return false;
  const role = (table.getAttribute("role") ?? "").trim().toLowerCase();
  if (role === "presentation" || role === "none") return false;
  let widest = 0;
  let someRowHasMany = false;
  for (const row of Array.from(table.querySelectorAll("tr"))) {
    const cells = Array.from(row.children).filter((c) => tag(c) === "td" || tag(c) === "th");
    if (cells.length > 1) someRowHasMany = true;
    let columns = 0;
    for (const c of cells) {
      const n = Number.parseInt(c.getAttribute("colspan") ?? "1", 10);
      columns += Number.isInteger(n) && n >= 1 ? n : 1;
    }
    widest = Math.max(widest, columns);
  }
  return someRowHasMany && widest <= MAX_COLUMNS;
}

/** The children of `container` as blocks: block elements stay, and each run of
 * loose inline content (text, links, pictures, breaks) becomes a paragraph. A run
 * that is only whitespace is dropped. */
function blocksOf(doc: Document, container: Node): Node[] {
  const out: Node[] = [];
  let run: Node[] = [];
  const flush = (): void => {
    const meaningful = run.some(
      (n) => (n.nodeType === TEXT ? (n.textContent ?? "").trim() !== "" : tag(n) !== "br"),
    );
    if (meaningful) {
      const p = doc.createElement("p");
      for (const n of run) p.appendChild(n);
      out.push(p);
    }
    run = [];
  };
  for (const child of Array.from(container.childNodes)) {
    if (isBlock(child)) {
      flush();
      out.push(child);
    } else if (child.nodeType === ELEMENT || child.nodeType === TEXT) {
      run.push(child);
    }
  }
  flush();
  return out;
}

/** A layout table's content, in document order: each cell's content as blocks. */
function flatten(doc: Document, table: Element): Node[] {
  const owned = (el: Element): boolean => el.closest("table") === table;
  const cells = Array.from(table.querySelectorAll("caption, td, th")).filter(owned);
  const out: Node[] = [];
  for (const cell of cells) out.push(...blocksOf(doc, cell));
  return out;
}

/** Tables, outermost first: a data table stays, anything else is replaced by its
 * cells' content. A table moved out of a flattened one is judged on its own. */
function handleTables(ctx: Ctx, root: Element): void {
  for (const table of Array.from(root.querySelectorAll("table"))) {
    if (!root.contains(table)) continue;
    if (isDataTable(table)) continue;
    table.replaceWith(...flatten(ctx.doc, table));
  }
}

interface Budget {
  text: number;
  nodes: number;
}

interface Cost {
  text: number;
  nodes: number;
  /** True when the walk stopped early because the node is already over budget. */
  over: boolean;
}

/** What one node costs against what is left: its text in code points and its
 * elements, measured by a walk that STOPS as soon as either passes the budget.
 * Measuring a whole subtree at every wrapper level was quadratic in the nesting
 * depth (500 nested <div>s around 1 MB of text took minutes), which is the very
 * hang the budget exists to prevent. A text node longer than twice what is left is
 * over without being counted, since it has at least half as many code points as
 * UTF-16 units. */
function cost(n: Node, left: Budget): Cost {
  const c: Cost = { text: 0, nodes: 0, over: false };
  const visit = (node: Node): boolean => {
    if (node.nodeType === TEXT) {
      const s = node.nodeValue ?? "";
      if (s.length > 2 * (left.text - c.text)) {
        c.over = true;
        return false;
      }
      c.text += codePoints(s);
    } else if (isElement(node)) {
      c.nodes += 1;
    }
    if (c.text > left.text || c.nodes > left.nodes) {
      c.over = true;
      return false;
    }
    for (let k = node.firstChild; k; k = k.nextSibling) if (!visit(k)) return false;
    return true;
  };
  visit(n);
  return c;
}

function spend(c: Cost, b: Budget): void {
  b.text -= c.text;
  b.nodes -= c.nodes;
}

/** Inline runs a cut may split between (never inside): a paragraph's own children,
 * which is where `<br>`-separated mail keeps its lines. */
const SPLITTABLE_TAGS = new Set(["p"]);

/** Keeps the leading children of an over-budget paragraph while they fit, cutting
 * BETWEEN children (a text node, a link, a `<br>`), never inside one. Returns
 * whether anything worth keeping is left. */
function fitInline(block: Element, budget: Budget): boolean {
  budget.nodes -= 1;
  const kids = Array.from(block.childNodes);
  let i = 0;
  for (; i < kids.length; i += 1) {
    const c = cost(kids[i]!, budget);
    if (c.over) break;
    spend(c, budget);
  }
  for (const rest of kids.slice(i)) rest.remove();
  return (block.textContent ?? "").trim() !== "" || block.querySelector("img") !== null;
}

/** Keeps whole blocks while the running text and element count stay within the
 * budget. A wrapper that does not fit whole is entered, its loose text first made
 * into paragraphs (the security review of #92: loose text inside a wrapper was
 * never counted, so one `<div>` around a whole message passed in full). An
 * over-budget paragraph is cut between its children. Returns false when something
 * was left out. */
function fit(doc: Document, container: Element, budget: Budget): boolean {
  const children = Array.from(container.children);
  for (let i = 0; i < children.length; i += 1) {
    const child = children[i] as Element;
    const c = cost(child, budget);
    if (!c.over) {
      spend(c, budget);
      continue;
    }
    // Does not fit whole.
    if (WRAPPER_TAGS.has(tag(child))) {
      child.replaceChildren(...blocksOf(doc, child));
      budget.nodes -= 1;
      fit(doc, child, budget);
      if (child.children.length === 0 && (child.textContent ?? "").trim() === "") child.remove();
      for (const rest of children.slice(i + 1)) rest.remove();
      return false;
    }
    if (SPLITTABLE_TAGS.has(tag(child)) && fitInline(child, budget)) {
      for (const rest of children.slice(i + 1)) rest.remove();
      return false;
    }
    for (const rest of children.slice(i)) rest.remove();
    return false;
  }
  return true;
}

/** Whether the document holds anything of the original's: text that is not one
 * of our own notes, or a picture that stays. */
function hasContent(ctx: Ctx, root: Element): boolean {
  if (root.querySelector("img")) return true;
  const walker = ctx.doc.createTreeWalker(root, 4 /* NodeFilter.SHOW_TEXT */);
  for (let n = walker.nextNode(); n; n = walker.nextNode()) {
    if (!ctx.notes.has(n) && (n.textContent ?? "").trim() !== "") return true;
  }
  return false;
}

/**
 * The quoted blocks of an HTML original, or `html: ""` when there is nothing to
 * quote. See the file header for the rules.
 */
export function formatQuote(input: QuoteInput): Quote {
  const doc = new DOMParser().parseFromString(input.html, "text/html");
  const body = doc.body;
  const ctx: Ctx = { doc, input, carried: [], carriedBytes: 0, kept: 0, notes: new Set() };

  handleHidden(body);
  for (const img of Array.from(body.querySelectorAll("img"))) handleImage(ctx, img);
  handleTables(ctx, body);

  // Loose text and inline runs at the top level become paragraphs, so that every
  // top-level node is a block the size budget can keep or leave out whole.
  const blocks = blocksOf(doc, body);
  const root = doc.createElement("div");
  for (const b of blocks) root.appendChild(b);

  if (!hasContent(ctx, root)) return { html: "", carried: [] };

  let complete = fit(doc, root, { text: QUOTE_TEXT_BUDGET, nodes: QUOTE_NODE_BUDGET });
  if (input.truncated) complete = false;
  if (!complete) {
    const p = doc.createElement("p");
    p.appendChild(doc.createTextNode(input.notes.rest));
    root.appendChild(p);
  }

  // A picture whose block was cut is no longer in the document: do not carry it.
  const inDoc = new Set(Array.from(root.querySelectorAll("img")).map((i) => cidKey(cidOf(i.getAttribute("src") ?? ""))));
  const carried = ctx.carried.filter((c) => inDoc.has(cidKey(c.cid)));

  const ser = new XMLSerializer();
  const html = Array.from(root.childNodes)
    .map((n) => ser.serializeToString(n))
    .join("");
  return { html, carried };
}
