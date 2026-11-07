// SPDX-License-Identifier: AGPL-3.0-only

// DISPLAY-ONLY PICTURE PREVIEWS FOR THE EDITOR.
//
// The document names an inline picture as `cid:...` (that is what is saved and
// sent). The app's CSP has no `cid:` source, so the browser would refuse to show
// one. The editor's image node view therefore shows a `data:` URL taken from this
// store instead, while the document itself keeps `cid:`. The store lives OUTSIDE
// the document: nothing in it is ever written into the saved or sent HTML.

import { normalizeCid } from "../inlineImages";

export interface PreviewStore {
  /** The data URL for a cid, or null when there is none (yet). */
  get: (cid: string) => string | null;
  has: (cid: string) => boolean;
  set: (cid: string, dataUrl: string) => void;
  /** Calls fn whenever a preview is set; returns the unsubscribe. */
  subscribe: (fn: (cid: string) => void) => () => void;
}

export function createPreviewStore(): PreviewStore {
  const map = new Map<string, string>();
  const subs = new Set<(cid: string) => void>();
  return {
    get: (cid) => map.get(normalizeCid(cid)) ?? null,
    has: (cid) => map.has(normalizeCid(cid)),
    set: (cid, dataUrl) => {
      const key = normalizeCid(cid);
      map.set(key, dataUrl);
      for (const fn of [...subs]) fn(key);
    },
    subscribe: (fn) => {
      subs.add(fn);
      return () => {
        subs.delete(fn);
      };
    },
  };
}

/** The most a preview may weigh: a larger picture keeps its alt text. */
export const MAX_PREVIEW_BYTES = 10 * 1024 * 1024;

/** Raster image types only: SVG is a document, not a picture. */
export function isPreviewableType(type: string | null | undefined): boolean {
  const v = (type ?? "").trim().toLowerCase();
  return v.startsWith("image/") && !v.startsWith("image/svg");
}
