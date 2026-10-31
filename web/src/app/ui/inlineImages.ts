// SPDX-License-Identifier: AGPL-3.0-only

// INLINE (cid:) IMAGE RESOLUTION — the client half of Forgejo #99.
//
// A `multipart/related` mail embeds its own images (a signature logo, a header
// banner) as MIME parts and references them from the HTML with `cid:` URLs
// (RFC 2392). Those bytes are ATTACHED to the message; displaying one fetches
// nothing external, so it is not "remote content" (#39) and must render without
// the "Show remote content" gate.
//
// HOW, without opening the remote hole. The server sanitizer keeps a `cid:` src
// verbatim (internal/api/sanitize.go classifies it as srcInline — inline bytes,
// not a fetch) and it already exposes, on every EmailBodyPart, the part's `cid`
// (its Content-ID) and `blobId` (minted by body.go's ONE part-id walk). So this
// module never re-parses the message: it takes the cid→blobId map straight off
// the body-part objects, fetches each referenced part from OUR OWN, session-
// authenticated download endpoint (a same-origin fetch by the app itself), and
// rewrites `src="cid:x"` to a `data:` URL of those bytes.
//
// A `data:` image is exactly the inline-bytes case the message CSP already
// permits (`img-src data:`). So resolving cid images needs NO new CSP source —
// no `img-src *`, no `https:`, no scoped self — and therefore cannot re-enable
// remote tracking. That is the whole reason we rehydrate to `data:` rather than
// pointing the sandboxed (opaque-origin) iframe at the download URL directly.

import type { AttachmentScan, EmailBodyPart } from "../../core/jmap/types";
import { isWithheld } from "./attachmentScanPolicy";

/** One inline part: the normalized Content-ID, the blobId that names its
 * decoded bytes, and the partId the quarantine keys its verdict on. */
export interface InlinePart {
  cid: string;
  blobId: string;
  partId: string | null;
}

/** Normalizes a Content-ID / `cid:` reference for matching. RFC 2392: a `cid:`
 * URL names a Content-ID with the enclosing angle brackets removed. internal/mime
 * already strips the `<>` off the header, but we strip again (defensively) and
 * lower-case so a header `<Logo@x>` and a reference `cid:logo@x` resolve to one
 * key. */
export function normalizeCid(raw: string): string {
  let s = raw.trim();
  if (s.startsWith("<") && s.endsWith(">")) s = s.slice(1, -1);
  return s.trim().toLowerCase();
}

/** Collects the cid → part map from the body-part objects the server already
 * emits. Each carries `cid` (Content-ID) and `blobId` (body.go's part-id walk),
 * so "the cid the message references" and "the part the client fetches" both come
 * from the server's single walk and cannot drift. The HTML is deliberately NOT
 * re-parsed for this. */
export function collectCidParts(
  ...lists: (EmailBodyPart[] | undefined)[]
): Map<string, InlinePart> {
  const out = new Map<string, InlinePart>();
  for (const list of lists) {
    for (const p of list ?? []) {
      if (!p.cid || !p.blobId) continue;
      const key = normalizeCid(p.cid);
      if (key && !out.has(key)) out.set(key, { cid: key, blobId: p.blobId, partId: p.partId });
    }
  }
  return out;
}

// Every attribute the server serializes is preceded by a space and its value has
// every `"` escaped to `&#34;` (Go's html.EscapeString), so ` src="cid:` occurs
// in exactly one place — an <img> src the SERVER wrote — and never in attacker
// text or in `data-mailroom-remote-src="` (that has `-src`, no space before it).
// This is the same soundness argument as messageDocument.ts's restoreRemoteContent.
const CID_SRC = / src="cid:([^"]*)"/gi;

/** The (normalized) cids a sanitized body actually references. */
export function referencedCids(html: string): string[] {
  const out: string[] = [];
  for (const m of html.matchAll(CID_SRC)) out.push(normalizeCid(htmlUnescape(m[1] ?? "")));
  return out;
}

/**
 * The inline parts a body references that the app may FETCH: each referenced
 * cid once, resolved against the server's body-part map, MINUS every part the
 * quarantine holds (#675).
 *
 * The download route refuses a held part anyway (internal/api/blob.go), so this
 * is not the gate — it is the client not asking a question whose answer is
 * already known to be no. A held part's `cid:` src is then stripped like any
 * other unresolved one (rewriteCidImages), and the reading pane lists the part
 * with its withheld chip instead (messageParts.ts), which is where the reader
 * learns it was held and can release it.
 */
export function fetchableInlineRefs(
  html: string,
  scan: AttachmentScan | null | undefined,
  ...lists: (EmailBodyPart[] | undefined)[]
): InlinePart[] {
  const map = collectCidParts(...lists);
  const seen = new Set<string>();
  const refs: InlinePart[] = [];
  for (const cid of referencedCids(html)) {
    const part = map.get(cid);
    if (!part || seen.has(cid)) continue;
    seen.add(cid);
    if (isWithheld(scan, part.partId)) continue;
    refs.push(part);
  }
  return refs;
}

/** Rewrites every `cid:` image src to its resolved `data:` URL. A cid that
 * resolves to no such part — or to one the download endpoint refused to serve as
 * a raster image — has its src STRIPPED, so the image renders as nothing rather
 * than as a dangling `cid:` reference. Remote (`data-mailroom-remote-src`) and
 * `data:` images are untouched. */
export function rewriteCidImages(html: string, resolved: Map<string, string>): string {
  return html.replace(CID_SRC, (_whole, escapedCid: string) => {
    const url = resolved.get(normalizeCid(htmlUnescape(escapedCid)));
    // A data: URL never contains a `"`, so it is safe to inline unescaped.
    return url ? ` src="${url}"` : "";
  });
}

/** Reads a fetched image part as a `data:` URL. */
export function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => reject(reader.error ?? new Error("inline image read failed"));
    reader.readAsDataURL(blob);
  });
}

/** Reverses the handful of entities html.EscapeString produces, so a captured
 * cid matches the raw Content-ID on the body-part object. */
function htmlUnescape(s: string): string {
  return s.replace(
    /&(amp|lt|gt|quot|apos|#34|#39);/g,
    (_m, e: string) =>
      ({ amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", "#34": '"', "#39": "'" })[e] ?? _m,
  );
}
