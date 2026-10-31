// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A HELD INLINE IMAGE IN THE READING PANE (#675), rendered for real.
//
// The message: an HTML body that references two cid: images, one of which the
// quarantine holds, plus an image whose Content-ID nothing references. Before
// #675 the held one vanished from the body and appeared nowhere else, and the
// unreferenced one appeared nowhere at all. What must hold now:
//
//   - the held image has a WITHHELD chip with the release action, and that
//     action releases THAT part;
//   - the unreferenced image is listed as an ordinary file;
//   - the held image's bytes are never asked for, while the displayed one's are
//     (the positive control that keeps "never asked for" from passing because
//     nothing was asked for at all);
//   - the held image's cid: reference does not survive into the frame.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { AttachmentScan, Email, EmailBodyPart } from "../../core/jmap/types";

const fetchInlineImage = vi.fn(async (_blobId: string) => new Blob([new Uint8Array([137, 80, 78, 71])], { type: "image/png" }));
const client = {
  fetchInlineImage,
  imageProxyEnabled: () => false,
  blobUrl: (blobId: string) => `/jmap/download/A/${blobId}`,
  fetchProxiedImage: vi.fn(async () => null),
};
const releaseAttachment = vi.fn(async () => true);
const actions = new Proxy(
  { releaseAttachment },
  { get: (t, k) => (k in t ? t[k as keyof typeof t] : vi.fn(async () => true)) },
);
const openViewer = vi.fn();

vi.mock("../state", () => ({
  useApp: () => ({ client, actions }),
  useMailState: () => ({ remoteAllows: {} }),
}));
vi.mock("./shellContext", () => ({
  useShell: () => ({ openViewer }),
}));

import { AttachmentList, HtmlBody } from "./ThreadPane";
import { listedParts } from "./messageParts";

function part(partId: string, type: string, extra: Partial<EmailBodyPart> = {}): EmailBodyPart {
  return { partId, blobId: `B.${partId}`, size: 2048, name: null, type, charset: null, disposition: null, cid: null, ...extra };
}
const htmlPart = part("1.2", "text/html");
const shown = part("2", "image/png", { cid: "<shown@example.com>", name: "logo.png" });
const held = part("3", "image/png", { cid: "<held@example.com>", name: "banner.png" });
const unref = part("4", "image/gif", { cid: "<unref@example.com>", name: "spacer.gif" });
const body =
  '<p>Hello</p><img src="cid:shown@example.com" alt="logo"><img src="cid:held@example.com" alt="banner">';
const scan: AttachmentScan = {
  parts: {
    "2": { state: "clean", released: false, releasedAt: null },
    "3": { state: "flagged", released: false, releasedAt: null },
    "4": { state: "clean", released: false, releasedAt: null },
  },
  quarantined: ["3"],
};
const email = {
  id: "M1",
  threadId: "T1",
  attachments: [],
  htmlBody: [htmlPart, shown, held, unref],
  textBody: [part("1.1", "text/plain")],
  bodyValues: { "1.2": { value: body, isEncodingProblem: false, isTruncated: false } },
  attachmentScan: scan,
} as unknown as Email;

let host: HTMLDivElement;
let root: Root;
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  fetchInlineImage.mockClear();
  releaseAttachment.mockClear();
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
});

describe("a held inline image (#675)", () => {
  it("gets a withheld chip whose release action releases that part", async () => {
    flushSync(() => {
      root.render(<AttachmentList messageId="M1" threadId="T1" attachments={listedParts(email)} scan={scan} />);
    });
    const chips = [...host.querySelectorAll<HTMLElement>(".attachment-withheld")];
    expect(chips.map((c) => c.getAttribute("aria-label"))).toEqual(["banner.png — withheld"]);
    const chip = chips[0] as HTMLElement;
    // A withheld chip offers no download: nothing in it links to the bytes.
    expect(chip.querySelector("a")).toBeNull();

    const release = [...chip.querySelectorAll("button")].find((b) => b.textContent === "Release");
    expect(release).toBeDefined();
    flushSync(() => release?.click());
    const confirm = [...document.querySelectorAll<HTMLButtonElement>("button")].find(
      (b) => b.textContent === "Release anyway",
    );
    expect(confirm).toBeDefined();
    flushSync(() => confirm?.click());
    await settle();
    expect(releaseAttachment).toHaveBeenCalledWith("T1", "M1", "3");
  });

  it("lists the image nothing references as an ordinary file", () => {
    flushSync(() => {
      root.render(<AttachmentList messageId="M1" threadId="T1" attachments={listedParts(email)} scan={scan} />);
    });
    const names = [...host.querySelectorAll(".attachments li:not(:has(.attachment-withheld)) .attachment-name")].map(
      (n) => n.textContent,
    );
    expect(names).toEqual(["spacer.gif"]);
  });

  it("is never fetched, and its cid: reference does not reach the frame", async () => {
    flushSync(() => {
      root.render(
        <HtmlBody
          html={body}
          title="t"
          htmlParts={email.htmlBody}
          attachments={email.attachments}
          scan={scan}
          senderAddress="news@example.com"
          messageId="M1"
        />,
      );
    });
    for (let i = 0; i < 20 && !host.querySelector("iframe")?.getAttribute("srcdoc")?.includes("data:image/png"); i++) {
      await settle();
    }
    // The positive control: the displayed image WAS fetched and inlined.
    expect(fetchInlineImage.mock.calls.map((c) => c[0])).toEqual(["B.2"]);
    const doc = host.querySelector("iframe")?.getAttribute("srcdoc") ?? "";
    expect(doc).toContain("data:image/png");
    expect(doc).not.toContain("cid:held@example.com");
    expect(doc).not.toContain("B.3");
  });

  it("keeps its cid: reference out of the frame when it is the ONLY inline image", async () => {
    // Nothing is fetchable here, so no fetch will ever arrive to rewrite the
    // body — the strip has to happen without one.
    const onlyHeld = '<p>Hello</p><img src="cid:held@example.com" alt="banner">';
    flushSync(() => {
      root.render(
        <HtmlBody
          html={onlyHeld}
          title="t"
          htmlParts={[htmlPart, held]}
          attachments={[]}
          scan={scan}
          senderAddress="news@example.com"
          messageId="M1"
        />,
      );
    });
    await settle();
    await settle();
    expect(fetchInlineImage).not.toHaveBeenCalled();
    const doc = host.querySelector("iframe")?.getAttribute("srcdoc") ?? "";
    expect(doc).toContain("Hello");
    expect(doc).not.toContain("cid:held@example.com");
  });
});
