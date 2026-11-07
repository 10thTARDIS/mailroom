// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// Pictures in the composer are DISPLAYED from data: URLs (the CSP has no cid:
// source) while every saved and sent body keeps cid:.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.setConfig({ testTimeout: 60_000 });

const identity = { id: "I1", accountId: "A1", email: "me@example.com", name: "Me", isWildcard: false };
const mailState: { identities: Record<string, unknown>; mailboxes: object; emails: object; composeSnippets: unknown[]; session: null } = {
  identities: { I1: identity },
  mailboxes: {},
  emails: {},
  composeSnippets: [],
  session: null,
};
const actions = {
  sendMessage: vi.fn(async () => null),
  discardDraft: vi.fn(async () => true),
  saveDraft: vi.fn(async () => ({ ok: false as const, reason: "test", retiredPrevious: false })),
  cancelSubmission: vi.fn(async () => true),
  releaseAttachment: vi.fn(),
};
const PNG_BYTES = new Uint8Array([137, 80, 78, 71]);
const fetchInlineImage = vi.fn(async (_id: string): Promise<Blob | null> => new Blob([PNG_BYTES], { type: "image/png" }));
const upload = vi.fn(async (f: File) => ({ blobId: "UP1", type: f.type, size: f.size }));
const client = {
  hasCapability: () => false,
  pgpSigning: () => null,
  pgpEncrypt: () => null,
  pgpKeyExpiryWarningDays: () => null,
  recipientKeyLookup: async () => null,
  contactForAddress: async () => null,
  fetchInlineImage,
  upload,
};
vi.mock("../state", () => ({
  useApp: () => ({ actions, client, pushToast: vi.fn(), offerUndo: vi.fn() }),
  useMailState: () => mailState,
}));
vi.mock("./useListVerbs", () => ({ requestPaneLeave: vi.fn() }));

import "./editor/jsdomShim";
import { Compose } from "./Compose";

const part = (over: Record<string, unknown>) => ({
  partId: "1", blobId: "B1", size: 100, name: null, type: "text/html", charset: null, disposition: null, cid: null, ...over,
});
const htmlPart = part({ partId: "h", blobId: "BH", type: "text/html" });
const textPart = part({ partId: "t", blobId: "BT", type: "text/plain" });
const logo = (over: Record<string, unknown> = {}) =>
  part({ partId: "i", blobId: "BI", type: "image/png", name: "logo.png", cid: "logo@example.com", size: 500, disposition: "inline", ...over });

function parentOf(html: string, parts: unknown[]): Record<string, unknown> {
  return {
    id: "P1", threadId: "T1", mailboxIds: {}, keywords: {}, isSeen: true, messageId: ["<p1@example.com>"], references: [],
    from: [{ name: "Alice", email: "alice@example.net" }], to: [{ name: null, email: "me@example.com" }], cc: [],
    subject: "the plan", receivedAt: "2026-03-02T10:00:00Z", textBody: [textPart], htmlBody: [htmlPart],
    bodyValues: {
      h: { value: html, isEncodingProblem: false, isTruncated: false },
      t: { value: "Hi", isEncodingProblem: false, isTruncated: false },
    },
    attachments: parts,
  };
}

let root: Root;
let host: HTMLElement;
const settle = (): Promise<void> => new Promise((r) => setTimeout(r, 15));
async function until(cond: () => boolean, what: string): Promise<void> {
  for (let i = 0; i < 1500; i += 1) {
    if (cond()) return;
    await act(async () => settle());
  }
  throw new Error(`timed out waiting for ${what}`);
}
async function open(intent: Record<string, unknown>): Promise<void> {
  await act(async () => {
    root.render(<Compose intent={intent as never} placement="full" scope={null} onClose={vi.fn()} />);
  });
  await until(() => host.querySelector(".ProseMirror") !== null, "the editor");
}
const imgs = (): HTMLImageElement[] => Array.from(host.querySelectorAll(".ProseMirror img:not([class])"));
async function sent(): Promise<string> {
  actions.sendMessage.mockClear();
  const form = host.querySelector("form") as HTMLFormElement;
  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  const call = actions.sendMessage.mock.calls[0] as unknown as [{ html?: string }] | undefined;
  if (!call) throw new Error("nothing was sent");
  return call[0].html ?? "";
}

beforeEach(() => {
  vi.clearAllMocks();
  fetchInlineImage.mockImplementation(async () => new Blob([PNG_BYTES], { type: "image/png" }));
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
});
afterEach(async () => {
  await act(async () => root.unmount());
  host.remove();
});

const QUOTE_HTML = '<p>Hi</p><p><img src="cid:logo@example.com" alt="Logo"></p>';
const reply = (parts: unknown[], html = QUOTE_HTML) => ({ kind: "reply", parent: parentOf(html, parts) });

describe("a quoted picture", () => {
  it("is fetched once and shown as a data: URL, while the sent body keeps cid:", async () => {
    await open(reply([logo()]));
    await until(() => imgs()[0]?.getAttribute("src")?.startsWith("data:image/png") === true, "the preview");
    expect(fetchInlineImage.mock.calls.map((c) => c[0])).toEqual(["BI"]);
    const html = await sent();
    expect(html).toContain('src="cid:logo@example.com"');
    expect(html).not.toContain("data:");
  });

  it("an SVG part gets no preview and no fetch", async () => {
    await open(reply([logo({ type: "image/svg+xml" })]));
    await act(async () => settle());
    expect(fetchInlineImage).not.toHaveBeenCalled();
    expect(imgs().every((i) => !i.hasAttribute("src"))).toBe(true);
  });

  it("a blob that is not an image gets no preview", async () => {
    fetchInlineImage.mockImplementation(async () => new Blob(["<svg/>"], { type: "image/svg+xml" }));
    await open(reply([logo()]));
    await until(() => fetchInlineImage.mock.calls.length === 1, "the fetch");
    await act(async () => settle());
    expect(imgs().every((i) => !i.hasAttribute("src"))).toBe(true);
  });

  it("a failed fetch leaves the alt text", async () => {
    fetchInlineImage.mockImplementation(async () => {
      throw new Error("nope");
    });
    await open(reply([logo()]));
    await until(() => fetchInlineImage.mock.calls.length === 1, "the fetch");
    await act(async () => settle());
    expect(imgs()[0]?.hasAttribute("src")).toBe(false);
  });
});

describe("a resumed draft", () => {
  const draft = (html: string, parts: unknown[]) => ({ kind: "draft", draft: parentOf(html, parts) });

  it("fetches once per cid and shows it; the saved HTML keeps cid:", async () => {
    await open(draft(QUOTE_HTML, [logo()]));
    await until(() => imgs()[0]?.getAttribute("src")?.startsWith("data:image/png") === true, "the preview");
    expect(fetchInlineImage).toHaveBeenCalledTimes(1);
    const html = await sent();
    expect(html).toContain('src="cid:logo@example.com"');
    expect(html).not.toContain("data:");
  });

  it("a cid with no inlineImages entry fetches nothing", async () => {
    await open(draft('<p><img src="cid:stray@example.com" alt="x"></p>', [logo()]));
    await until(() => fetchInlineImage.mock.calls.length === 1, "the matched fetch");
    await act(async () => settle());
    expect(fetchInlineImage.mock.calls.map((c) => c[0])).toEqual(["BI"]);
    expect(imgs()[0]?.hasAttribute("src")).toBe(false);
  });
});

describe("an inserted picture", () => {
  it("is shown from the chosen file with no fetch, and the send carries cid: only", async () => {
    await open(reply([], "<p>Hi</p>"));
    const input = host.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File([PNG_BYTES], "cat.png", { type: "image/png" });
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await act(async () => {
      input.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await until(() => imgs()[0]?.getAttribute("src")?.startsWith("data:image/png") === true, "the preview");
    expect(fetchInlineImage).not.toHaveBeenCalled();
    const html = await sent();
    expect(html).toMatch(/src="cid:img-[^"]+@inline\.mailroom"/);
    expect(html).not.toContain("data:");
  });
});
