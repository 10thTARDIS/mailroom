// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";

import type { AttachmentScan, EmailBodyPart } from "../../core/jmap/types";
import { fetchableInlineRefs } from "./inlineImages";
import { listedParts } from "./messageParts";

function part(partId: string, type: string, extra: Partial<EmailBodyPart> = {}): EmailBodyPart {
  return {
    partId,
    blobId: `B.${partId}`,
    size: 10,
    name: null,
    type,
    charset: null,
    disposition: null,
    cid: null,
    ...extra,
  };
}

const html = part("1", "text/html");
const shown = part("2", "image/png", { cid: "<shown@x>", name: "shown.png" });
const held = part("3", "image/png", { cid: "<held@x>", name: "held.png" });
const unref = part("4", "image/gif", { cid: "<unref@x>", name: "unref.gif" });
const pdf = part("5", "application/pdf", { name: "doc.pdf", disposition: "attachment" });

const body = '<p>hi</p><img src="cid:shown@x" alt="s"><img src="cid:held@x" alt="h">';
const values = { "1": { value: body, isEncodingProblem: false, isTruncated: false } };
function scanHolding(...partIds: string[]): AttachmentScan {
  const parts: AttachmentScan["parts"] = {};
  for (const p of ["2", "3", "4", "5"]) {
    parts[p] = { state: partIds.includes(p) ? "flagged" : "clean", released: false, releasedAt: null };
  }
  return { parts, quarantined: partIds };
}

const ids = (ps: EmailBodyPart[]): (string | null)[] => ps.map((p) => p.partId);

describe("listedParts (#675)", () => {
  it("lists a HELD inline image the body references, after the attachments", () => {
    const got = listedParts({
      attachments: [pdf],
      htmlBody: [html, shown, held],
      textBody: [part("0", "text/plain")],
      bodyValues: values,
      attachmentScan: scanHolding("3"),
    });
    expect(ids(got)).toEqual(["5", "3"]);
  });

  it("lists a held inline image before the body has loaded — held does not depend on it", () => {
    const got = listedParts({ attachments: [], htmlBody: [html, shown, held], attachmentScan: scanHolding("3") });
    expect(ids(got)).toEqual(["3"]);
  });

  it("lists an image whose Content-ID the HTML never references, once the body is loaded", () => {
    const src = { attachments: [pdf], htmlBody: [html, shown, unref], attachmentScan: scanHolding() };
    // Not yet loaded: every image would read as unreferenced, so nothing is decided.
    expect(ids(listedParts(src))).toEqual(["5"]);
    expect(ids(listedParts({ ...src, bodyValues: values }))).toEqual(["5", "4"]);
  });

  it("does not list an inline image the body displays", () => {
    const got = listedParts({ htmlBody: [html, shown], bodyValues: values, attachmentScan: scanHolding() });
    expect(got).toEqual([]);
  });

  it("matches cids the way the renderer does (brackets and case)", () => {
    const upper = part("6", "image/png", { cid: "<SHOWN@X>" });
    const got = listedParts({ htmlBody: [html, upper], bodyValues: values });
    expect(got).toEqual([]);
  });

  it("lists every inline image when the pane shows the plain-text body, and each part once", () => {
    // multipart/mixed with no HTML: the image sits in BOTH body lists.
    const text = part("1", "text/plain");
    const img = part("2", "image/png", { cid: "<pic@x>" });
    const got = listedParts({
      htmlBody: [text, img],
      textBody: [text, img],
      bodyValues: { "1": { value: "", isEncodingProblem: false, isTruncated: false } },
    });
    expect(ids(got)).toEqual(["2"]);
  });

  it("never lists a part twice when it is already an attachment", () => {
    const got = listedParts({ attachments: [held], htmlBody: [html, held], bodyValues: values, attachmentScan: scanHolding("3") });
    expect(ids(got)).toEqual(["3"]);
  });

  it("is the attachments array itself when nothing is added", () => {
    const attachments = [pdf];
    expect(listedParts({ attachments, htmlBody: [html, shown], bodyValues: values })).toBe(attachments);
  });
});

describe("fetchableInlineRefs (#675)", () => {
  it("never asks for a held inline part, and still asks for the others", () => {
    const refs = fetchableInlineRefs(body, scanHolding("3"), [html, shown, held]);
    expect(refs.map((r) => r.blobId)).toEqual(["B.2"]);
  });

  it("asks for every referenced part when nothing is held", () => {
    const refs = fetchableInlineRefs(body, scanHolding(), [html, shown, held]);
    expect(refs.map((r) => r.blobId)).toEqual(["B.2", "B.3"]);
  });

  it("a released part is fetched again — release is what reopens the door", () => {
    const scan = scanHolding();
    scan.parts["3"] = { state: "flagged", released: true, releasedAt: "2026-10-01T00:00:00Z" };
    const refs = fetchableInlineRefs(body, scan, [html, shown, held]);
    expect(refs.map((r) => r.blobId)).toEqual(["B.2", "B.3"]);
  });
});
