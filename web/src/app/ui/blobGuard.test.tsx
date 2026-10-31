// SPDX-License-Identifier: AGPL-3.0-only
// @vitest-environment jsdom

// A BLOCKED DOWNLOAD IS EXPLAINED IN THE APP, NOT NAVIGATED TO (#47).
//
// The reported defect: Download on an attachment of a decrypted message that
// clamd flags sent the browser to a bare "403 Forbidden" page. The fix asks the
// server first (a HEAD) and navigates only when it would serve the file.
//
// "No navigation" is observed as: no anchor click is performed. The throwaway
// anchor the guard uses to hand a served URL to the browser is the only thing
// that navigates, so a spy on HTMLAnchorElement.click sees it. The positive
// control (a served file DOES click) keeps "never clicked" from passing because
// nothing could click at all.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";

import type { BlobProbe } from "../../core/jmap/blobs";
import type { EmailBodyPart } from "../../core/jmap/types";

const probeBlob = vi.fn(async (_url: string): Promise<BlobProbe> => ({ kind: "ok" }));
const fetchPart = vi.fn();
const client = {
  probeBlob,
  fetchPart,
  blobUrl: (blobId: string) => `/jmap/download/A/${blobId}`,
  previewUrl: (blobId: string) => `/jmap/preview/A/${blobId}`,
};
vi.mock("../state", () => ({
  useApp: () => ({ client, actions: {} }),
  useMailState: () => ({ remoteAllows: {} }),
}));
vi.mock("./shellContext", () => ({
  useShell: () => ({ openViewer: vi.fn() }),
}));

import { AttachmentList } from "./ThreadPane";
import { PreviewBody } from "./attachmentPreview";
import { blockedMessage } from "./blobGuard";

function part(partId: string, type: string, name: string): EmailBodyPart {
  return { partId, blobId: `B.${partId}`, size: 2048, name, type, charset: null, disposition: "attachment", cid: null };
}

let host: HTMLDivElement;
let root: Root;
let clicked: string[];
/** The USER's click. Not link.click(): that is the method the spy replaces. */
const userClick = (el: HTMLElement): void => {
  el.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
};
const settle = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

beforeEach(() => {
  host = document.createElement("div");
  document.body.appendChild(host);
  root = createRoot(host);
  probeBlob.mockReset();
  fetchPart.mockReset();
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this.getAttribute("href") ?? "");
  });
});
afterEach(() => {
  flushSync(() => root.unmount());
  host.remove();
  vi.restoreAllMocks();
});

function renderList(): HTMLAnchorElement {
  // application/zip has no preview, so the chip itself is the download link.
  const zip = part("2", "application/zip", "archive.zip");
  flushSync(() => {
    root.render(<AttachmentList messageId="M1" threadId="T1" attachments={[zip]} scan={null} />);
  });
  const link = host.querySelector<HTMLAnchorElement>("a.attachment");
  expect(link).not.toBeNull();
  return link as HTMLAnchorElement;
}

describe("a refused download (#47)", () => {
  it("shows the verdict and the detection name in the app, and does not navigate", async () => {
    probeBlob.mockResolvedValue({ kind: "blocked", blocked: { verdict: "flagged", signature: "Eicar-Test-Signature" } });
    const link = renderList();
    userClick(link);
    await settle();
    const alert = host.querySelector('[role="alert"]');
    expect(alert?.textContent).toBe("Blocked: the virus scanner flagged this attachment (Eicar-Test-Signature).");
    expect(clicked).toEqual([]);
    expect(probeBlob).toHaveBeenCalledWith("/jmap/download/A/B.2");
  });

  it("says plainly that an unscannable attachment was not scanned", async () => {
    probeBlob.mockResolvedValue({ kind: "blocked", blocked: { verdict: "unknown", signature: null } });
    userClick(renderList());
    await settle();
    expect(host.querySelector('[role="alert"]')?.textContent).toMatch(/^Blocked: .*not scanned/);
    expect(clicked).toEqual([]);
  });

  it("does not navigate to a failed request either", async () => {
    probeBlob.mockResolvedValue({ kind: "failed" });
    userClick(renderList());
    await settle();
    expect(host.querySelector('[role="alert"]')?.textContent).toMatch(/could not be fetched/);
    expect(clicked).toEqual([]);
  });

  it("POSITIVE CONTROL: a served file is handed to the browser, with no message", async () => {
    probeBlob.mockResolvedValue({ kind: "ok" });
    userClick(renderList());
    await settle();
    expect(clicked).toEqual(["/jmap/download/A/B.2"]);
    expect(host.querySelector('[role="alert"]')).toBeNull();
  });
});

describe("a refused preview (#47)", () => {
  it("a PDF is not framed when the server refuses it; the panel says why", async () => {
    probeBlob.mockResolvedValue({ kind: "blocked", blocked: { verdict: "flagged", signature: "Eicar-Test-Signature" } });
    flushSync(() => {
      root.render(<PreviewBody part={part("2", "application/pdf", "plan.pdf")} />);
    });
    await settle();
    expect(host.querySelector("iframe")).toBeNull();
    expect(host.querySelector('[role="status"]')?.textContent).toBe(
      "Blocked: the virus scanner flagged this attachment (Eicar-Test-Signature).",
    );
  });

  it("POSITIVE CONTROL: a served PDF is framed", async () => {
    probeBlob.mockResolvedValue({ kind: "ok" });
    flushSync(() => {
      root.render(<PreviewBody part={part("2", "application/pdf", "plan.pdf")} />);
    });
    await settle();
    expect(host.querySelector("iframe")?.getAttribute("src")).toBe("/jmap/preview/A/B.2");
  });

  it("an image the server refuses shows the reason instead of an empty preview", async () => {
    fetchPart.mockImplementation(async (_b, _a, _n, _m, onBlocked) => {
      onBlocked({ verdict: "unknown", signature: null });
      return null;
    });
    flushSync(() => {
      root.render(<PreviewBody part={part("2", "image/png", "pic.png")} />);
    });
    await settle();
    expect(host.querySelector("img")).toBeNull();
    expect(host.querySelector('[role="status"]')?.textContent).toMatch(/^Blocked: /);
  });
});

// ONE REQUEST PER ACTION ON A DECRYPTED PART (#804).
//
// For an attachment inside encrypted mail the server decrypts and scans on every
// request, so a pre-flight check followed by the real GET was two decryptions,
// two scans and two audit rows per click. The assertions below are those request
// counts: before the fix a download made one probe plus one browser GET (two);
// now it is exactly one fetch and no probe.
describe("a part of a decrypted message costs one request (#804)", () => {
  const HASH = "a".repeat(64);
  const decryptedUrl = `/jmap/download/A/${HASH}.e.2/doc.zip`;
  const fetchBlobOnce = vi.fn();
  let created: Blob[];

  function decryptedPart(type: string, name: string): EmailBodyPart {
    return { partId: "e.2", blobId: `${HASH}.e.2`, size: 2048, name, type, charset: null, disposition: "attachment", cid: null };
  }

  function renderDecrypted(): HTMLAnchorElement {
    flushSync(() => {
      root.render(
        <AttachmentList messageId="M1" threadId="T1" attachments={[decryptedPart("application/zip", "doc.zip")]} scan={null} />,
      );
    });
    return host.querySelector<HTMLAnchorElement>("a.attachment") as HTMLAnchorElement;
  }

  beforeEach(() => {
    fetchBlobOnce.mockReset();
    created = [];
    (client as Record<string, unknown>).fetchBlobOnce = fetchBlobOnce;
    client.blobUrl = (blobId: string, name?: string) => (name ? `/jmap/download/A/${blobId}/${name}` : `/jmap/download/A/${blobId}`);
    URL.createObjectURL = (b: Blob | MediaSource) => {
      created.push(b as Blob);
      return "blob:test/1";
    };
    URL.revokeObjectURL = () => {};
  });
  afterEach(() => {
    client.blobUrl = (blobId: string) => `/jmap/download/A/${blobId}`;
  });

  it("downloads with exactly one fetch and no probe", async () => {
    fetchBlobOnce.mockResolvedValue({ kind: "ok", blob: new Blob(["x"], { type: "application/octet-stream" }) });
    userClick(renderDecrypted());
    await settle();
    expect(fetchBlobOnce).toHaveBeenCalledTimes(1);
    expect(fetchBlobOnce).toHaveBeenCalledWith(decryptedUrl);
    expect(probeBlob).not.toHaveBeenCalled();
    expect(clicked).toEqual(["blob:test/1"]);
    expect(created[0]?.type).toBe("application/octet-stream");
    expect(host.querySelector('[role="alert"]')).toBeNull();
  });

  it("a refusal from that one fetch is explained in the app and saves nothing", async () => {
    fetchBlobOnce.mockResolvedValue({ kind: "blocked", blocked: { verdict: "flagged", signature: "Eicar-Test-Signature" } });
    userClick(renderDecrypted());
    await settle();
    expect(fetchBlobOnce).toHaveBeenCalledTimes(1);
    expect(probeBlob).not.toHaveBeenCalled();
    expect(host.querySelector('[role="alert"]')?.textContent).toBe(
      "Blocked: the virus scanner flagged this attachment (Eicar-Test-Signature).",
    );
    expect(clicked).toEqual([]);
    expect(created).toEqual([]);
  });

  it("NEGATIVE CONTROL: a stored part is still checked first (the cheap path is kept)", async () => {
    probeBlob.mockResolvedValue({ kind: "ok" });
    userClick(renderList());
    await settle();
    expect(probeBlob).toHaveBeenCalledTimes(1);
    expect(fetchBlobOnce).not.toHaveBeenCalled();
  });

  it("a decrypted PDF is framed without a probe", async () => {
    flushSync(() => {
      root.render(<PreviewBody part={decryptedPart("application/pdf", "plan.pdf")} />);
    });
    await settle();
    expect(probeBlob).not.toHaveBeenCalled();
    expect(host.querySelector("iframe")?.getAttribute("src")).toBe(`/jmap/preview/A/${HASH}.e.2`);
  });
});

describe("blockedMessage", () => {
  it("flagged without a detection name still says flagged", () => {
    expect(blockedMessage({ verdict: "flagged", signature: null })).toBe(
      "Blocked: the virus scanner flagged this attachment.",
    );
  });
});
