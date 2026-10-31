// SPDX-License-Identifier: AGPL-3.0-only

// What the viewer's thumbnail strip lists (#449).
//
// Two facts are worth a test here and they are different in kind. One is a
// COUNTING fact — the strip lists every attachment, so it cannot disagree with
// the paperclips in the reading pane about how many files a message has. The
// other is a SAFETY fact: a held file is marked BEFORE it is landed on, in the
// accessible name as well as in the flag a dashed outline is drawn from, so the
// mark is not carried by appearance alone.

import { describe, expect, it } from "vitest";

import { stepIndex, stripItems } from "./strip";
import type { AttachmentScan, EmailBodyPart } from "../../../core/jmap/types";

function part(over: Partial<EmailBodyPart> = {}): EmailBodyPart {
  return {
    partId: "2",
    blobId: "B1.2",
    size: 2048,
    name: "photo.png",
    type: "image/png",
    charset: null,
    disposition: "attachment",
    cid: null,
    ...over,
  };
}

const three: EmailBodyPart[] = [
  part({ partId: "2", blobId: "B.2", name: "photo.png", type: "image/png" }),
  part({ partId: "3", blobId: "B.3", name: "invoice.pdf", type: "application/pdf" }),
  part({ partId: "4", blobId: "B.4", name: "payload.zip", type: "application/zip" }),
];

const scan = (parts: AttachmentScan["parts"], quarantined: string[]): AttachmentScan => ({
  parts,
  quarantined,
});

describe("stripItems", () => {
  it("lists EVERY attachment, including the ones nothing can render", () => {
    // The lightbox this replaces paged only the previewable ones (#200). A .zip is
    // in the strip now because the viewer has a panel for it — and because a strip
    // that silently skipped it would count two files where the pane shows three.
    const model = stripItems(three, null, 0);
    expect(model.items.map((i) => i.name)).toEqual(["photo.png", "invoice.pdf", "payload.zip"]);
    expect(model.items.map((i) => i.glyph)).toEqual(["png", "pdf", "zip"]);
  });

  it("marks exactly one item as the current one, and says the position in words", () => {
    const model = stripItems(three, null, 1);
    expect(model.items.filter((i) => i.current).map((i) => i.index)).toEqual([1]);
    expect(model.position).toBe("2 of 3");
    expect(model.navigable).toBe(true);
  });

  // ---- PROBE 2's SUBJECT. Both halves of the mark, because they reach different
  // people: `held` is what the stylesheet draws the dashed outline from, and the
  // word in the accessible name is what a screen reader gets — colour and border
  // reach neither.
  it("marks a held attachment in the strip, in the flag AND in the accessible name", () => {
    const s = scan({ "3": { state: "flagged", released: false } }, ["3"]);
    const model = stripItems(three, s, 0);
    const held = model.items[1];
    expect(held?.held).toBe(true);
    expect(held?.label).toContain("held");
    expect(held?.label).toContain("invoice.pdf");
  });

  // The CONTROL for that one: without it, "held" could be what every item says
  // and the assertion above would still pass.
  it("does not mark the attachments the same message did not withhold", () => {
    const s = scan(
      {
        "2": { state: "clean", released: false },
        "3": { state: "flagged", released: false },
      },
      ["3"],
    );
    const model = stripItems(three, s, 0);
    expect(model.items.map((i) => i.held)).toEqual([false, true, false]);
    expect(model.items[0]?.label).not.toContain("held");
    expect(model.items[2]?.label).not.toContain("held");
  });

  it("strips bidi controls out of the name it hands the chrome", () => {
    // Spelled as a code point, never pasted (core/hostileText.ts).
    const rlo = String.fromCodePoint(0x202e);
    const model = stripItems([part({ name: `report${rlo}gnp.exe` })], null, 0);
    expect(model.items[0]?.name).toBe("reportgnp.exe");
    expect(model.items[0]?.label).not.toContain(rlo);
  });

  it("names an unnamed part by its type rather than by nothing", () => {
    const model = stripItems([part({ name: null, type: "application/pdf" })], null, 0);
    expect(model.items[0]?.name).toBe("application/pdf");
  });

  it("keeps a key for a part the server gave no blobId", () => {
    const model = stripItems([part({ blobId: null }), part({ blobId: null })], null, 0);
    expect(new Set(model.items.map((i) => i.key)).size).toBe(2);
  });

  it("reports no position for an index that names nothing, rather than inventing one", () => {
    // A bounded or empty result reports its bound (CLAUDE.md): "1 of 0" would be a
    // sentence about a file that is not there.
    expect(stripItems([], null, 0).position).toBe("");
    expect(stripItems(three, null, 7).position).toBe("");
    expect(stripItems(three, null, -1).position).toBe("");
    expect(stripItems([], null, 0).navigable).toBe(false);
    expect(stripItems([part()], null, 0).navigable).toBe(false);
  });
});

describe("stepIndex", () => {
  it("wraps in both directions", () => {
    expect(stepIndex(0, 1, 3)).toBe(1);
    expect(stepIndex(2, 1, 3)).toBe(0);
    expect(stepIndex(0, -1, 3)).toBe(2);
  });

  it("stays put with one attachment, and cannot go negative with none", () => {
    expect(stepIndex(0, 1, 1)).toBe(0);
    expect(stepIndex(0, -1, 1)).toBe(0);
    expect(stepIndex(0, -1, 0)).toBe(0);
  });
});
