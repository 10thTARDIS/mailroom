// SPDX-License-Identifier: AGPL-3.0-only

// WHAT AN ATTACHMENT PREVIEW IS ALLOWED TO RENDER (Forgejo #131, spec §11).
// The threat model is written down once in docs/hostile-content.md §2.4.
//
// This module is the POLICY half of the preview, split out from the React
// component on purpose: it is the part that decides what may be rendered at all,
// it is the part a mistake in would be a security bug rather than a layout bug,
// and being DOM-free it can be tested without a browser (the same reason
// core/needsCategory.ts is pure). The component in ./attachmentPreview.tsx owns
// only the fetching, the element and the object-URL lifecycle.
//
// The one rule to carry away: EVERY SET HERE IS AN ALLOWLIST OF TYPES THE SERVER
// CHOSE, never of types a message declared. internal/api/blob.go maps text/html,
// application/xhtml+xml, image/svg+xml and the XML family — every scripting
// context — onto application/octet-stream, so `application/octet-stream` is the
// bucket the dangerous types fall into and must NEVER appear in any set below.

// THERE IS DELIBERATELY NO IFRAME SANDBOX CONSTANT HERE, and the absence is the
// interesting part — it is where the first design of this feature died.
//
// The obvious build is: fetch the PDF, mint a `blob:` URL, frame it with
// `sandbox="allow-scripts"` so the document gets an opaque origin. Measured against
// real Chromium, that does not work at all: A SANDBOXED IFRAME CANNOT LOAD A BLOB:
// URL (net::ERR_BLOCKED_BY_CLIENT). The frame renders an error page, which looks
// exactly like a feature that works until someone opens a PDF. The only way to make
// a blob preview render is to remove the sandbox — i.e. to trade the origin boundary
// for the convenience of not touching the server.
//
// So the PDF is served by our own preview route instead
// (internal/api/blob.go PreviewHandler) and the opaque origin arrives in the
// RESPONSE: `Content-Security-Policy: default-src 'none'; sandbox`. Two things that
// buys, both verified in a browser: the guarantee holds however the embedding
// element is written, and adding a `sandbox` attribute would actually BREAK it —
// a sandboxed frame is an opaque origin to the server too, so the response's
// `Cross-Origin-Resource-Policy: same-origin` refuses the load
// (net::ERR_BLOCKED_BY_RESPONSE).
//
// If you are reading this because you want to sandbox that iframe: the sandbox is
// already there, one layer down, where it cannot be forgotten.

/** Bounds on what we will hand a renderer.
 *
 * A bound on the RENDERER, not a judgement about the attachment: an oversized part
 * is still perfectly downloadable, and the reason is shown rather than the button
 * silently doing nothing.
 *
 * Text has its own, far smaller bound because its cost differs in KIND: an image
 * or a PDF is handed to the browser, whereas text becomes DOM nodes in our own
 * document, and twenty-five megabytes of those would hang the tab. One bound for
 * both would have to be the smaller one, and would then refuse ordinary PDFs. */
export const PREVIEW_MAX_BYTES = 25 * 1024 * 1024;
export const PREVIEW_TEXT_MAX_BYTES = 2 * 1024 * 1024;

/** How much previewed text goes into the DOM at once. Past this the preview is
 * explicitly flagged as shortened — quietly showing the first slice of a long file
 * would be a lie about what the attachment contains. */
export const PREVIEW_TEXT_MAX_CHARS = 200_000;

/** The raster types a preview will render, as the SERVER may name them.
 *
 * The raster subset of blob.go's downloadableTypes. image/svg+xml is omitted on
 * purpose — SVG is a scripting context. That endpoint already refuses to serve SVG
 * under its own name, so this set and that allowlist would both have to be wrong
 * for an SVG to reach an <img>. */
export const PREVIEW_IMAGE_TYPES: ReadonlySet<string> = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "image/bmp",
  "image/avif",
  "image/tiff",
  "image/heic",
]);

/** `text/plain` only.
 *
 * Notably NOT text/csv or text/markdown: blob.go's allowlist does not name them,
 * so the server serves them as application/octet-stream — the same bucket
 * text/html and SVG land in, which must never be previewable. Rendering them would
 * mean either widening the server's allowlist or admitting octet-stream here, and
 * the second is not a trade worth making for a table view. A .csv that declares
 * text/plain — most of them do — previews as the text it is. */
export const PREVIEW_TEXT_TYPES: ReadonlySet<string> = new Set(["text/plain"]);

export const PREVIEW_PDF_TYPES: ReadonlySet<string> = new Set(["application/pdf"]);

export type PreviewKind = "image" | "text" | "pdf";

/** Which renderer, if any, would handle this part — from its DECLARED type,
 * because that is all we have before fetching anything.
 *
 * THIS IS NOT THE SECURITY CONTROL. It decides only whether to OFFER a preview.
 * The declared type is the sender's claim; the real gate is the type the server
 * serves, checked after the fetch (client.ts fetchPart). A part lying about being
 * a PDF earns a button, then either fails that gate or fails to parse — inside the
 * sandbox, having been served as the type we asked for and not as HTML. */
export function previewKind(declaredType: string | null | undefined): PreviewKind | null {
  const t = (declaredType ?? "").split(";")[0]?.trim().toLowerCase() ?? "";
  if (PREVIEW_IMAGE_TYPES.has(t)) return "image";
  if (PREVIEW_TEXT_TYPES.has(t)) return "text";
  if (PREVIEW_PDF_TYPES.has(t)) return "pdf";
  return null;
}

/** The attachments something can actually RENDER: the ones with a blobId AND a
 * renderer (#200).
 *
 * ⚠ THIS IS NO LONGER THE VIEWER'S RING, and the change is worth stating because
 * the old comment here said the opposite. Under the lightbox (#200) the ring WAS
 * this set, so that paging could never land on a dead end offering nothing. The
 * viewer (#449) pages through EVERY attachment on the message instead: it has
 * somewhere to land now — a panel with a Download in it for a type nothing renders,
 * and the two-weight explanation for a held file — and a strip that silently skipped
 * files would disagree with the paperclips in the reading pane about how many the
 * message has.
 *
 * What it still answers, and the only thing it answers, is what a CHIP in the
 * reading pane promises: a part in this set opens the viewer onto a rendered file, a
 * part outside it is a download link and says so. One helper for that, so a chip
 * cannot claim a preview the viewer would not produce. */
export function previewableParts<T extends { blobId: string | null; type: string }>(
  parts: readonly T[],
): T[] {
  return parts.filter((p) => p.blobId !== null && previewKind(p.type) !== null);
}

/** The gate and the bound that belong together, so a call site cannot pair a
 * renderer with another renderer's allowlist. */
export function rulesFor(kind: PreviewKind): {
  allowed: ReadonlySet<string>;
  maxBytes: number;
} {
  switch (kind) {
    case "image":
      return { allowed: PREVIEW_IMAGE_TYPES, maxBytes: PREVIEW_MAX_BYTES };
    case "text":
      return { allowed: PREVIEW_TEXT_TYPES, maxBytes: PREVIEW_TEXT_MAX_BYTES };
    case "pdf":
      return { allowed: PREVIEW_PDF_TYPES, maxBytes: PREVIEW_MAX_BYTES };
  }
}
