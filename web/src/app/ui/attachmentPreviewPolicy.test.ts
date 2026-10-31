// SPDX-License-Identifier: AGPL-3.0-only

// Tests for what an attachment preview is allowed to render (#131).
//
// These are security assertions, not behaviour checks. The preview's whole safety
// argument is "we render only a short list of inert types, named by the server" —
// so the thing worth pinning is the NEGATIVES: the types that must never become
// renderable, no matter how a message declares itself. A regression here would not
// look like a bug; it would look like a preview that suddenly handles more files.

import { describe, expect, it } from "vitest";

import {
  PREVIEW_IMAGE_TYPES,
  PREVIEW_MAX_BYTES,
  PREVIEW_PDF_TYPES,
  PREVIEW_TEXT_MAX_BYTES,
  PREVIEW_TEXT_TYPES,
  previewKind,
  previewableParts,
  rulesFor,
} from "./attachmentPreviewPolicy";

/** Every scripting context, plus the bucket the server maps them all into. If any
 * of these is ever previewable, a phishing attachment renders as a document. */
const MUST_NEVER_RENDER = [
  "text/html",
  "application/xhtml+xml",
  "image/svg+xml",
  "text/xml",
  "application/xml",
  "application/javascript",
  "text/javascript",
  // THE IMPORTANT ONE. internal/api/blob.go serves every type it does not
  // recognise — including all of the above — as application/octet-stream. If
  // octet-stream were previewable, the server's downgrade would stop protecting
  // anything and every unlisted type would render.
  "application/octet-stream",
];

describe("previewKind", () => {
  it("refuses every scripting context and the octet-stream bucket", () => {
    for (const type of MUST_NEVER_RENDER) {
      expect(previewKind(type), `${type} must not be previewable`).toBeNull();
      // ...and not via a parameter or casing trick either.
      expect(previewKind(`${type.toUpperCase()}; charset=utf-8`)).toBeNull();
    }
  });

  it("recognises the three renderers, ignoring parameters and case", () => {
    expect(previewKind("image/png")).toBe("image");
    expect(previewKind("IMAGE/JPEG")).toBe("image");
    expect(previewKind("text/plain; charset=utf-8")).toBe("text");
    expect(previewKind("application/pdf")).toBe("pdf");
  });

  it("treats a missing or empty type as not previewable", () => {
    expect(previewKind(null)).toBeNull();
    expect(previewKind(undefined)).toBeNull();
    expect(previewKind("")).toBeNull();
    expect(previewKind("   ")).toBeNull();
  });

  it("never claims a type no set contains", () => {
    expect(previewKind("application/zip")).toBeNull();
    expect(previewKind("application/vnd.openxmlformats-officedocument.wordprocessingml.document")).toBeNull();
    // text/csv and text/markdown are deliberately absent: the server does not name
    // them, so they arrive as octet-stream. See the module comment.
    expect(previewKind("text/csv")).toBeNull();
    expect(previewKind("text/markdown")).toBeNull();
  });
});

describe("the allowlists themselves", () => {
  it("contain no scripting context and no octet-stream", () => {
    for (const set of [PREVIEW_IMAGE_TYPES, PREVIEW_TEXT_TYPES, PREVIEW_PDF_TYPES]) {
      for (const type of MUST_NEVER_RENDER) {
        expect(set.has(type), `${type} is in a preview allowlist`).toBe(false);
      }
    }
  });

  it("keep SVG out of the image set even though it is an image type", () => {
    expect(PREVIEW_IMAGE_TYPES.has("image/svg+xml")).toBe(false);
  });
});

describe("rulesFor", () => {
  it("pairs each renderer with its own allowlist", () => {
    expect(rulesFor("image").allowed).toBe(PREVIEW_IMAGE_TYPES);
    expect(rulesFor("text").allowed).toBe(PREVIEW_TEXT_TYPES);
    expect(rulesFor("pdf").allowed).toBe(PREVIEW_PDF_TYPES);
  });

  it("bounds text far tighter than the rest, because text becomes our own DOM", () => {
    expect(rulesFor("text").maxBytes).toBe(PREVIEW_TEXT_MAX_BYTES);
    expect(rulesFor("text").maxBytes).toBeLessThan(rulesFor("pdf").maxBytes);
    expect(rulesFor("pdf").maxBytes).toBe(PREVIEW_MAX_BYTES);
  });

  it("bounds every renderer — an unbounded one would be fed whatever arrived", () => {
    for (const kind of ["image", "text", "pdf"] as const) {
      expect(rulesFor(kind).maxBytes).toBeGreaterThan(0);
      expect(Number.isFinite(rulesFor(kind).maxBytes)).toBe(true);
    }
  });
});

describe("previewableParts — the lightbox ring (#200)", () => {
  const part = (blobId: string | null, type: string) => ({ blobId, type });

  it("keeps only parts that have both a blobId and a renderer", () => {
    const parts = [
      part("b1", "application/pdf"),
      part("b2", "application/zip"), // no renderer
      part(null, "image/png"), // no blobId (a multipart node)
      part("b4", "image/png"),
      part("b5", "text/plain"),
    ];
    expect(previewableParts(parts).map((p) => p.blobId)).toEqual(["b1", "b4", "b5"]);
  });

  it("never admits a part the preview must refuse", () => {
    // The ring decides which chips OPEN A DIALOG rather than download. If a
    // scripting context reached it, the chip would stop saying what it does — and
    // the dialog would be the thing offering it.
    for (const type of MUST_NEVER_RENDER) {
      expect(previewableParts([part("b", type)])).toEqual([]);
    }
  });

  it("preserves order, because the ring IS the paging order", () => {
    const parts = [part("z", "text/plain"), part("a", "application/pdf")];
    expect(previewableParts(parts).map((p) => p.blobId)).toEqual(["z", "a"]);
  });

  it("returns an empty ring rather than throwing on no attachments", () => {
    expect(previewableParts([])).toEqual([]);
  });
});

// The wrap arithmetic the lightbox's prev/next uses. Written out here because an
// off-by-one in it is the difference between "next wraps to the first" and "next
// silently does nothing at the end", and both look fine in a screenshot.
describe("ring navigation arithmetic", () => {
  const go = (index: number, delta: number, len: number) => (index + delta + len) % len;

  it("wraps forward off the end and backward off the start", () => {
    expect(go(2, 1, 3)).toBe(0);
    expect(go(0, -1, 3)).toBe(2);
  });

  it("steps normally in the middle", () => {
    expect(go(1, 1, 3)).toBe(2);
    expect(go(1, -1, 3)).toBe(0);
  });

  it("stays put with a single attachment", () => {
    expect(go(0, 1, 1)).toBe(0);
    expect(go(0, -1, 1)).toBe(0);
  });
});
