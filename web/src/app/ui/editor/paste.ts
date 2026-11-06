// SPDX-License-Identifier: AGPL-3.0-only

// Rich paste (ADR-051 §4). ProseMirror parses the clipboard's HTML into the
// editor's schema in an inert document: elements and attributes the schema does
// not define are gone, and so are event handlers and disallowed link schemes. The
// one thing the schema would otherwise allow that we do not is a picture that
// lives somewhere else; the Image extension's parse rule refuses those on every
// parse path (./extensions.ts). This module only COUNTS them, so the user can be
// told once.

import { isInlineImageSrc } from "./uri";

/** How many `<img>` in pasted markup are not inline pictures (cid:, or raster
 * data:image/*). Parsed with DOMParser, which builds an inert document: nothing
 * loads and no handler runs. */
export function countDroppedImages(html: string): number {
  const doc = new DOMParser().parseFromString(html, "text/html");
  let n = 0;
  doc.querySelectorAll("img").forEach((img) => {
    if (!isInlineImageSrc(img.getAttribute("src"))) n += 1;
  });
  return n;
}
