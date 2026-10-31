// SPDX-License-Identifier: AGPL-3.0-only

// The viewer's three pure decisions (#449): which mode, which held panel, and
// what the strip's kind word may say.
//
// Tested here rather than through a rendered Viewer for the reason
// attachmentPreviewPolicy.test.ts states: this repo has no jsdom and no React
// Testing Library, so a decision that matters has to live somewhere a test can
// reach. These three do:
//
//   * a held part reaching a renderer would show a blank pane where the reason it
//     is held belongs — and 403 on every byte it asked for;
//   * a flagged panel whose primary button is the release is one reflex away from
//     opening malware;
//   * a kind glyph taken raw off a sender's filename is attacker text in our own
//     chrome.

import { describe, expect, it } from "vitest";

import { heldPanelSpec, isHeldMode, kindGlyph, releasedNote, viewerMode } from "./kinds";
import type { AttachmentScan, AttachmentScanPart, EmailBodyPart } from "../../../core/jmap/types";

/** RIGHT-TO-LEFT OVERRIDE, as core/hostileText.test.ts writes it. */
const RLO = String.fromCodePoint(0x202e);

function part(over: Partial<EmailBodyPart> = {}): EmailBodyPart {
  return {
    partId: "2",
    blobId: "B1.2",
    size: 1024,
    name: "photo.png",
    type: "image/png",
    charset: null,
    disposition: "attachment",
    cid: null,
    ...over,
  };
}

function scan(parts: Record<string, AttachmentScanPart>, quarantined: string[]): AttachmentScan {
  return { parts, quarantined };
}

describe("viewerMode", () => {
  it("names the renderer for a part nothing is withholding", () => {
    expect(viewerMode(part(), null)).toBe("image");
    expect(viewerMode(part({ type: "application/pdf", name: "a.pdf" }), null)).toBe("pdf");
    expect(viewerMode(part({ type: "text/plain", name: "a.txt" }), null)).toBe("text");
  });

  it("has no preview for a type the server will not serve as a renderable one", () => {
    // application/octet-stream is the bucket blob.go downgrades text/html, SVG and
    // the XML family into — it must never be renderable.
    expect(viewerMode(part({ type: "application/octet-stream", name: "a.html" }), null)).toBe(
      "nopreview",
    );
    expect(viewerMode(part({ type: "application/zip", name: "a.zip" }), null)).toBe("nopreview");
  });

  it("has no preview for a part with no blobId — there are no bytes to ask for", () => {
    expect(viewerMode(part({ blobId: null, type: "multipart/mixed" }), null)).toBe("nopreview");
  });

  // ---- THE ONE THIS FILE EXISTS FOR (probe 1). A held IMAGE is the case that
  // fails silently if the checks are in the wrong order: previewKind says "image",
  // the <img> asks the download route for bytes, and the route answers 403 — so
  // the viewer shows an empty frame instead of the reason the file is held.
  it("shows the held panel for a held image, NOT the image", () => {
    const s = scan({ "2": { state: "unknown", released: false } }, ["2"]);
    expect(viewerMode(part(), s)).toBe("held-unscanned");
  });

  it("separates the scanner's verdict from its failure to reach one", () => {
    const flagged = scan({ "2": { state: "flagged", released: false } }, ["2"]);
    const unknown = scan({ "2": { state: "unknown", released: false } }, ["2"]);
    const unscanned = scan({ "2": { state: "unscanned", released: false } }, ["2"]);
    expect(viewerMode(part(), flagged)).toBe("held-flagged");
    expect(viewerMode(part(), unknown)).toBe("held-unscanned");
    expect(viewerMode(part(), unscanned)).toBe("held-unscanned");
  });

  // The CONTROL for the two assertions above: without it, "held" could be what
  // every part resolves to and they would both still pass.
  it("renders a part the same message quarantined a DIFFERENT part of", () => {
    const s = scan(
      {
        "2": { state: "clean", released: false },
        "3": { state: "flagged", released: false },
      },
      ["3"],
    );
    expect(viewerMode(part({ partId: "2" }), s)).toBe("image");
  });

  it("renders a released part — the hold is what a release lifts", () => {
    // The server drops it from `quarantined` on release; nothing here re-derives
    // that from state/released (attachmentScanPolicy.ts's ⚠).
    const s = scan({ "2": { state: "flagged", released: true } }, []);
    expect(viewerMode(part(), s)).toBe("image");
  });

  it("falls to the panel that does not accuse for a held state it does not know", () => {
    const s = scan({ "2": { state: "clean", released: false } }, ["2"]);
    expect(viewerMode(part(), s)).toBe("held-unscanned");
  });
});

describe("isHeldMode", () => {
  it("covers both held modes and nothing else", () => {
    expect(isHeldMode("held-flagged")).toBe(true);
    expect(isHeldMode("held-unscanned")).toBe(true);
    expect(isHeldMode("image")).toBe(false);
    expect(isHeldMode("pdf")).toBe(false);
    expect(isHeldMode("text")).toBe(false);
    expect(isHeldMode("nopreview")).toBe(false);
  });
});

describe("heldPanelSpec", () => {
  // ---- PROBE 3's SUBJECT. The primary button is the safe answer for a file the
  // scanner actually flagged, and the release asks once more on top of that.
  it("leads with Keep held for a flagged file, and asks again before releasing", () => {
    const spec = heldPanelSpec("flagged");
    expect(spec.primary).toBe("keep");
    expect(spec.secondary).toBe("release");
    expect(spec.asksAgain).toBe(true);
    expect(spec.tone).toBe("warn");
  });

  it("leads with the release for a file nothing could check, and does not ask twice", () => {
    for (const state of ["unknown", "unscanned"]) {
      const spec = heldPanelSpec(state);
      expect(spec.primary).toBe("release");
      expect(spec.secondary).toBe("keep");
      expect(spec.asksAgain).toBe(false);
      expect(spec.tone).toBe("held");
    }
  });

  it("gives the two cases different words, not one shared 'withheld' sentence", () => {
    const flagged = heldPanelSpec("flagged");
    const unknown = heldPanelSpec("unknown");
    expect(flagged.titleKey).not.toBe(unknown.titleKey);
    expect(flagged.bodyKey).not.toBe(unknown.bodyKey);
    expect(flagged.releaseKey).not.toBe(unknown.releaseKey);
  });

  it("treats an unrecognised state as unchecked rather than as flagged", () => {
    // Nothing else in the app can produce this, and if something ever does, the
    // wrong answer is the one that calls a file malware (ADR-027 §8).
    expect(heldPanelSpec(undefined).mode).toBe("held-unscanned");
    expect(heldPanelSpec("something-new").primary).toBe("release");
  });
});

describe("releasedNote", () => {
  it("names the date the user released it", () => {
    const note = releasedNote({
      state: "flagged",
      released: true,
      releasedAt: "2026-08-12T09:04:00Z",
    });
    // The SHAPE rather than a literal date: the day-of-month a UTC instant falls
    // on depends on the runtime's zone, and pinning "12 Aug" would make this test
    // a statement about where CI happens to be. Day-then-month with a locale month
    // name is the fixed part (desk/format.ts rowTime keeps the same order).
    expect(note).toMatch(/^released by you · \d{1,2} \S+ 2026$/u);
  });

  it("says it without a date when the server sent none", () => {
    // A server older than #484 sends no releasedAt at all. "released by you" is
    // still true; an invented or "Invalid Date" date is not.
    expect(releasedNote({ state: "flagged", released: true })).toBe("released by you");
    expect(releasedNote({ state: "flagged", released: true, releasedAt: null })).toBe(
      "released by you",
    );
    expect(releasedNote({ state: "flagged", released: true, releasedAt: "not a date" })).toBe(
      "released by you",
    );
  });

  // The CONTROL: the mark is a fact about a decision, so it must be absent when
  // there was none — otherwise every clean attachment would wear it.
  it("is nothing for a part nobody released", () => {
    expect(releasedNote({ state: "clean", released: false })).toBeNull();
    expect(releasedNote(null)).toBeNull();
    expect(releasedNote(undefined)).toBeNull();
  });
});

describe("kindGlyph", () => {
  it("prefers the file's own extension, in the word people read", () => {
    expect(kindGlyph("image/jpeg", "holiday.JPG")).toBe("jpg");
    expect(kindGlyph("image/jpeg", "holiday.jpeg")).toBe("jpg");
    expect(kindGlyph("application/pdf", "invoice.pdf")).toBe("pdf");
    expect(kindGlyph("application/zip", "logs.zip")).toBe("zip");
    expect(kindGlyph("application/gzip", "archive.tar.gz")).toBe("gz");
    expect(
      kindGlyph(
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "budget.xlsx",
      ),
    ).toBe("xlsx");
  });

  it("uses the SERVER's type when the name has no usable extension", () => {
    expect(kindGlyph("application/pdf", "scan")).toBe("pdf");
    expect(kindGlyph("image/png", null)).toBe("png");
    expect(kindGlyph("image/webp", "")).toBe("webp");
    expect(kindGlyph("text/plain", "notes")).toBe("txt");
    expect(
      kindGlyph("application/vnd.oasis.opendocument.text", "letter"),
    ).toBe("odt");
  });

  // ---- HOSTILE INPUT. The name is chosen by the sender and this glyph is drawn
  // in our own chrome, beside every other one.
  it("refuses an extension that is not glyph-shaped and falls back to the type", () => {
    // A bidi override in the extension would reorder the box it is drawn in.
    // Spelled as a CODE POINT, never pasted in: core/hostileText.ts explains at
    // length why a module about invisible characters must not contain any.
    expect(kindGlyph("application/pdf", `invoice.p${RLO}df`)).toBe("pdf");
    // A sentence is not an extension.
    expect(kindGlyph("application/pdf", "report.this-is-not-an-extension")).toBe("pdf");
    // Neither is 40 characters of one.
    expect(kindGlyph("image/png", `shot.${"a".repeat(40)}`)).toBe("png");
    // Nor a script-looking one.
    expect(kindGlyph("image/png", "x.<b>")).toBe("png");
  });

  it("says only 'file' for the bucket the dangerous types are downgraded into", () => {
    // blob.go maps text/html, XHTML, SVG and the XML family to
    // application/octet-stream. Printing "html" there would name a type the
    // server refused to serve, on a file that cannot be rendered at all.
    expect(kindGlyph("application/octet-stream", "page")).toBe("file");
    expect(kindGlyph(null, null)).toBe("file");
    expect(kindGlyph("", "")).toBe("file");
  });

  it("ignores a charset parameter on the type", () => {
    expect(kindGlyph("text/plain; charset=utf-8", "notes")).toBe("txt");
  });
});
