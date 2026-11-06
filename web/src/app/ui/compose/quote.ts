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

/** The content-id a `cid:` source names, decoded when it is percent-encoded. */
function cidOf(src: string): string {
  const raw = urlView(src).slice(4).replace(/^<|>$/g, "");
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function plainCid(cid: string | null): string {
  return (cid ?? "").trim().replace(/^<|>$/g, "");
}

interface Ctx {
  doc: Document;
  input: QuoteInput;
  carried: CarriedPicture[];
  carriedBytes: number;
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
  if (view.startsWith("cid:")) {
    const kept = keepCid(ctx, img, cidOf(src));
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
  const already = ctx.carried.find((c) => c.cid === cid);
  const part = ctx.input.parts.find((p) => plainCid(p.cid) === cid && p.blobId);
  if (!part || !part.blobId) return false;
  const type = part.type.toLowerCase();
  if (!type.startsWith("image/") || type.startsWith("image/svg")) return false;
  if (ctx.input.withheld(part.partId)) return false;
  if (!already) {
    if (ctx.carriedBytes + part.size > QUOTE_PICTURE_BYTE_CAP) return false;
    ctx.carriedBytes += part.size;
    ctx.carried.push({
      blobId: part.blobId,
      cid,
      name: part.name ?? "image",
      type: part.type,
      partId: part.partId,
    });
  }
  stripImage(img);
  img.setAttribute("src", `cid:${cid}`);
  return true;
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

/** Keeps whole blocks while the running text stays within the budget. A wrapper
 * that does not fit whole is entered, so one big `<div dir="ltr">` around a long
 * message still yields its leading blocks. Returns false when something was left
 * out. */
function fit(container: Element, state: { left: number }): boolean {
  const children = Array.from(container.children);
  for (let i = 0; i < children.length; i += 1) {
    const child = children[i] as Element;
    const len = textLength(child);
    if (len <= state.left) {
      state.left -= len;
      continue;
    }
    // Does not fit whole.
    if (WRAPPER_TAGS.has(tag(child))) {
      fit(child, state);
      if (child.children.length === 0 && (child.textContent ?? "").trim() === "") child.remove();
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
  const ctx: Ctx = { doc, input, carried: [], carriedBytes: 0, notes: new Set() };

  for (const img of Array.from(body.querySelectorAll("img"))) handleImage(ctx, img);
  handleTables(ctx, body);

  // Loose text and inline runs at the top level become paragraphs, so that every
  // top-level node is a block the size budget can keep or leave out whole.
  const blocks = blocksOf(doc, body);
  const root = doc.createElement("div");
  for (const b of blocks) root.appendChild(b);

  if (!hasContent(ctx, root)) return { html: "", carried: [] };

  let complete = fit(root, { left: QUOTE_TEXT_BUDGET });
  if (input.truncated) complete = false;
  if (!complete) {
    const p = doc.createElement("p");
    p.appendChild(doc.createTextNode(input.notes.rest));
    root.appendChild(p);
  }

  // A picture whose block was cut is no longer in the document: do not carry it.
  const inDoc = new Set(Array.from(root.querySelectorAll("img")).map((i) => cidOf(i.getAttribute("src") ?? "")));
  const carried = ctx.carried.filter((c) => inDoc.has(c.cid));

  const ser = new XMLSerializer();
  const html = Array.from(root.childNodes)
    .map((n) => ser.serializeToString(n))
    .join("");
  return { html, carried };
}
